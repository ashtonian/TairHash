set testmodule [file normalize your_path/tairhash_module.so]

# Entries of an EXHSYNCGET reply as sorted {field value version} triples, so replies
# compare independently of hash iteration order.
proc sync_entries {reply} {
    set out {}
    foreach {f v ver} [lindex $reply 1] {
        lappend out [list $f $v $ver]
    }
    return [lsort -index 0 $out]
}

proc synced {client key} {
    lindex [$client exhsyncget $key] 0
}

# Major and minor version of the server under test (Valkey reports a compatible redis_version).
proc tairhash_server_at_least {major minor} {
    lassign [lrange [split [s redis_version] .] 0 1] ma mi
    expr {$ma > $major || ($ma == $major && $mi >= $minor)}
}

start_server {tags {"tairhash lww"} overrides {bind 0.0.0.0}} {
    r module load $testmodule

    test {EXHMSETLWW applies higher versions and reports losing writes} {
        r del k
        assert_equal {1 1} [r exhmsetlww k f1 a 10 f2 b 10]
        assert_equal {0 1} [r exhmsetlww k f1 old 9 f2 new 11]
        assert_equal {a 10} [r exhgetwithver k f1]
        assert_equal {new 11} [r exhgetwithver k f2]
    }

    test {EXHMSETLWW breaks version ties by the byte-wise greater value} {
        r del k
        r exhmsetlww k f b 5
        assert_equal {0} [r exhmsetlww k f a 5]
        assert_equal {0} [r exhmsetlww k f b 5]
        assert_equal {1} [r exhmsetlww k f c 5]
        assert_equal {1} [r exhmsetlww k f cc 5]
        assert_equal {cc 5} [r exhgetwithver k f]

        # The tie-break makes the outcome independent of arrival order.
        r del k
        r exhmsetlww k f cc 5
        r exhmsetlww k f c 5
        r exhmsetlww k f a 5
        assert_equal {cc 5} [r exhgetwithver k f]
    }

    test {EXHMSETLWW compares binary values as unsigned bytes} {
        r del k
        r exhmsetlww k f "\x7f" 1
        assert_equal {1} [r exhmsetlww k f "\x80" 1]
        assert_equal {0} [r exhmsetlww k f "\x00\xff" 1]
    }

    test {EXHMSETLWW applies repeated fields in argument order} {
        r del k
        assert_equal {1 1 0} [r exhmsetlww k f a 1 f b 2 f c 1]
        assert_equal {b 2} [r exhgetwithver k f]
    }

    test {EXHMSETLWW clears the TTL of a field it overwrites and keeps it on a losing write} {
        r del k
        r exhset k f v ex 100
        assert_equal {1} [r exhmsetlww k f v2 1000000]
        assert_equal -1 [r exhttl k f]

        r exhset k g v ex 100 abs 50
        assert_equal {0} [r exhmsetlww k g w 10]
        assert {[r exhttl k g] > 0}
        assert_equal v [r exhget k g]
    }

    test {EXHMSETLWW rejects malformed input before writing anything} {
        r del k
        assert_error {*wrong number of arguments*} {r exhmsetlww k f v}
        assert_error {*wrong number of arguments*} {r exhmsetlww k f v 1 g}
        assert_error {*syntax*} {r exhmsetlww k f v 1 g w 0}
        assert_error {*syntax*} {r exhmsetlww k f v 1 g w -3}
        assert_error {*syntax*} {r exhmsetlww k f v 1 g w nope}
        assert_equal 0 [r exists k]

        r set str x
        assert_error {*WRONGTYPE*} {r exhmsetlww str f v 1}
    }

    test {EXHDELLWW deletes at the same or a higher version and keeps newer writes} {
        r del k
        r exhmsetlww k f1 a 10 f2 b 10 f3 c 10
        assert_equal {1 1 0} [r exhdellww k f1 10 f2 11 f3 9]
        assert_equal {f3} [r exhkeys k]
        assert_equal {c 10} [r exhgetwithver k f3]
        # Deleting an already deleted field at the same or a lower version is in effect.
        assert_equal {1 1} [r exhdellww k f1 10 f2 3]
    }

    test {EXHDELLWW leaves a tombstone that writes up to its version lose to} {
        r del k
        r exhmsetlww k f a 10 keep x 1
        assert_equal {1} [r exhdellww k f 12]
        assert_equal {0 0} [r exhmsetlww k f late 11 f tie 12]
        assert_equal 0 [r exhexists k f]
        assert_equal {1} [r exhmsetlww k f new 13]
        assert_equal {new 13} [r exhgetwithver k f]
        # The landed write dropped the tombstone: once the field is gone without one, any
        # version lands again.
        r exhdel k f
        assert_equal {1} [r exhmsetlww k f older 5]
    }

    test {EXHDELLWW of a missing key keeps the tombstones in a hash that expires with them} {
        r del k
        assert_equal {1} [r exhdellww k f 5]
        assert_equal 1 [r exists k]
        assert_equal 0 [r exhlen k]
        set pttl [r pttl k]
        assert {$pttl > 3500000 && $pttl <= 3600000}
        assert_equal {0} [r exhmsetlww k f old 5]
        assert_equal 1 [r exists k]

        # A write that adds a field makes it a regular hash again.
        assert_equal {1} [r exhmsetlww k f v 6]
        assert_equal -1 [r ttl k]
    }

    test {EXHDELLWW that empties a hash with a TTL of its own keeps that TTL} {
        r del k
        r exhmsetlww k f v 1
        r expire k 100000
        assert_equal {1} [r exhdellww k f 1]
        assert_equal 1 [r exists k]
        assert {[r ttl k] > 99000}
        assert_equal {1} [r exhmsetlww k f v 2]
        assert {[r ttl k] > 99000}
    }

    test {Any write that adds a field removes the TTL of a hash with only tombstones} {
        foreach cmd {
            {exhset k g v}
            {exhsetnx k g v}
            {exhmset k g v}
            {exhmsetwithopts k g v 0 100}
            {exhincrby k g 1}
            {exhincrbyfloat k g 1.5}
        } {
            r del k
            r exhdellww k f 1
            assert {[r pttl k] > 0}
            r {*}$cmd
            assert_equal -1 [r ttl k]
        }
    }

    test {EXHDELLWW rejects malformed input before deleting anything} {
        r del k
        r exhmsetlww k f v 1
        assert_error {*wrong number of arguments*} {r exhdellww k}
        assert_error {*wrong number of arguments*} {r exhdellww k f}
        assert_error {*wrong number of arguments*} {r exhdellww k f 1 g}
        assert_error {*syntax*} {r exhdellww k f 1 g 0}
        assert_error {*syntax*} {r exhdellww k f 1 g nope}
        assert_equal {f} [r exhkeys k]

        r set str x
        assert_error {*WRONGTYPE*} {r exhdellww str f 1}
    }

    if {[tairhash_server_at_least 6 2]} {
        test {The internal LWW commands are rejected from clients} {
            r del k
            assert_error {*replication stream*} {r exhsyncrepl k 0 0}
            assert_error {*replication stream*} {r exhtombrepl k 1 f 1 1}
            assert_equal 0 [r exists k]
        }
    }

    test {Tombstones and the TTL of a hash with only tombstones survive DEBUG RELOAD} {
        r flushall
        r exhmsetlww deleted keep v 1 f v 1
        r exhdellww deleted f 7
        r exhdellww gone f 7
        set gone_pttl [r pttl gone]
        r debug reload
        assert_equal {0} [r exhmsetlww deleted f old 7]
        assert_equal {0} [r exhmsetlww gone f old 7]
        assert {[r pttl gone] > 0 && [r pttl gone] <= $gone_pttl}
        # It is still a hash with only tombstones: a write that adds a field removes its TTL.
        r exhset gone g v
        assert_equal -1 [r ttl gone]
    }

    test {Tombstones survive AOF replay and AOF rewrite} {
        r flushall
        r config set aof-use-rdb-preamble no
        r config set appendonly yes
        waitForBgrewriteaof r
        assert_equal ok [s aof_last_bgrewrite_status]

        # Replayed from the incremental AOF, i.e. the propagated effects.
        r exhmsetlww deleted keep v 1 f v 1
        r exhdellww deleted f 7
        r exhdellww gone f 7
        set check {
            assert_equal {0} [r exhmsetlww deleted f old 7]
            assert_equal {0} [r exhmsetlww gone f old 7]
            assert {[r pttl gone] > 0}
        }
        r debug loadaof
        eval $check
        set digest [r debug digest]

        # Replayed from the rewritten base, i.e. TairHashTypeAofRewrite.
        r bgrewriteaof
        waitForBgrewriteaof r
        assert_equal ok [s aof_last_bgrewrite_status]
        r debug loadaof
        assert_equal $digest [r debug digest]
        eval $check
        r exhset gone g v
        assert_equal -1 [r ttl gone]

        r config set appendonly no
        r config set aof-use-rdb-preamble yes
    }
}

# The sync commands need the server events of Redis 6.0+.
start_server {tags {"tairhash lww sync"} overrides {bind 0.0.0.0}} {
    r module load $testmodule
    if {![tairhash_server_at_least 6 0]} {
        test {The sync commands are left out without server events} {
            assert_error {*unknown command*} {r exhsyncbegin k}
            assert_error {*unknown command*} {r exhsyncget k}
        }
    } else {

        test {EXHSYNCGET reports a missing or never-synced hash as a miss} {
            r del k
            assert_equal {0 {}} [r exhsyncget k]
            r exhmsetlww k f v 1
            assert_equal {0 {}} [r exhsyncget k]
            assert_equal {0 {}} [r exhsyncget k f]
        }

        test {EXHSYNCBEGIN creates a placeholder that expires with the lease} {
            r del k
            set token [r exhsyncbegin k]
            assert {$token > 0}
            assert_equal 1 [r exists k]
            assert_equal 0 [r exhlen k]
            set pttl [r pttl k]
            assert {$pttl > 50000 && $pttl <= 60000}
            assert_equal 0 [synced r k]
        }

        test {EXHSYNCBEGIN leaves the TTL of an existing hash alone} {
            r del k
            r exhmsetlww k f v 1
            assert_equal -1 [r ttl k]
            r exhsyncbegin k
            assert_equal -1 [r ttl k]
            r expire k 100000
            r exhsyncbegin k
            assert {[r ttl k] > 99000}
        }

        test {Concurrent syncs share the lease and only the first EXHSYNC completes} {
            r del k
            set t1 [r exhsyncbegin k]
            set t2 [r exhsyncbegin k]
            assert_equal $t1 $t2
            assert_equal {1 0} [r exhsync k $t1 f a 1]
            assert_error {*EXHSYNCBEGIN*} {r exhsync k $t2 f b 2}
            assert_equal {{f a 1}} [sync_entries [r exhsyncget k]]
            # The next sync gets a new lease.
            assert {[r exhsyncbegin k] > $t1}
        }

        test {EXHSYNC merges the snapshot, marks the hash synced and makes the placeholder permanent} {
            r del k
            assert_equal {2 0} [r exhsync k [r exhsyncbegin k] f1 a 10 f2 b 20]
            assert_equal -1 [r ttl k]
            assert_equal 1 [synced r k]
            assert_equal {{f1 a 10} {f2 b 20}} [sync_entries [r exhsyncget k]]
        }

        test {EXHSYNC keeps a TTL the hash had of its own} {
            r del k
            r exhmsetlww k f v 1
            r expire k 100000
            r exhsync k [r exhsyncbegin k] f v 1
            assert {[r ttl k] > 99000}
            assert_equal 1 [synced r k]
        }

        test {EXHSYNC keeps newer cached writes and removes fields the source no longer has} {
            r del k
            r exhsync k [r exhsyncbegin k] f1 a 10 f2 b 20 gone x 5
            # The source now holds f2 at an older version than the cache, and no 'gone'.
            assert_equal {0 1} [r exhsync k [r exhsyncbegin k] f1 a 10 f2 old 15]
            assert_equal {{f1 a 10} {f2 b 20}} [sync_entries [r exhsyncget k]]
        }

        test {EXHSYNC keeps a field written after the token even when its version is old} {
            r del k
            r exhsync k [r exhsyncbegin k] f1 a 10
            set token [r exhsyncbegin k]
            # Raced the snapshot read: a late-arriving write carrying an old version.
            r exhmsetlww k late v 1
            assert_equal {0 0} [r exhsync k $token f1 a 10]
            assert_equal {{f1 a 10} {late v 1}} [sync_entries [r exhsyncget k]]

            # A sync opened after that write removes it if the source lacks it.
            assert_equal {0 1} [r exhsync k [r exhsyncbegin k] f1 a 10]
            assert_equal {{f1 a 10}} [sync_entries [r exhsyncget k]]
        }

        test {EXHSYNC fencing covers every write command} {
            r del k
            r exhsync k [r exhsyncbegin k] base v 1
            set token [r exhsyncbegin k]
            r exhset k a v
            r exhsetnx k b v
            r exhmset k c v
            r exhmsetwithopts k d v 0 100
            r exhincrby k e 1
            r exhincrbyfloat k f 1.5
            assert_equal {0 0} [r exhsync k $token base v 1]
            assert_equal 7 [r exhlen k]
        }

        test {A delete that races the snapshot read is not undone by EXHSYNC} {
            r del k
            r exhsync k [r exhsyncbegin k] f a 5 g b 5
            set token [r exhsyncbegin k]
            # The snapshot was read before the source deleted f; the delete reaches the cache first.
            r exhdellww k f 6
            assert_equal {0 0} [r exhsync k $token f a 5 g b 5]
            assert_equal {{g b 5}} [sync_entries [r exhsyncget k]]
        }

        test {EXHSYNC fails once the key went away during the sync} {
            r del k
            set token [r exhsyncbegin k]
            r del k
            assert_error {*EXHSYNCBEGIN*} {r exhsync k $token f a 1}
            assert_equal 0 [r exists k]

            # Nor does a lease of a key recreated in the meantime accept the old token.
            set token [r exhsyncbegin k]
            r del k
            set fresh [r exhsyncbegin k]
            assert_error {*EXHSYNCBEGIN*} {r exhsync k $token f a 1}
            assert_equal {1 0} [r exhsync k $fresh f b 2]
        }

        test {EXHSYNC requires an open lease} {
            r del k
            r exhmsetlww k f v 1
            assert_error {*EXHSYNCBEGIN*} {r exhsync k 12345 f a 10}
            set token [r exhsyncbegin k]
            assert_error {*EXHSYNCBEGIN*} {r exhsync k [expr {$token + 1}] f a 10}
            assert_equal {f} [r exhkeys k]
            assert_equal 0 [synced r k]
        }

        test {A hash with an open sync survives losing its last field} {
            r del k
            r exhmsetlww k f v 1
            set token [r exhsyncbegin k]
            r exhdel k f
            assert_equal 1 [r exists k]
            assert {[r pttl k] > 0}
            assert_equal {1 0} [r exhsync k $token g w 1]
            assert_equal -1 [r ttl k]
        }

        test {EXHSYNC with an empty snapshot caches an empty synced hash} {
            r del k
            assert_equal {0 0} [r exhsync k [r exhsyncbegin k]]
            assert_equal 1 [r exists k]
            assert_equal -1 [r ttl k]
            assert_equal 0 [r exhlen k]
            assert_equal {1 {}} [r exhsyncget k]
        }

        test {A synced hash survives losing its last field, an unsynced one does not} {
            r del synced plain
            r exhsync synced [r exhsyncbegin synced] f v 1
            r exhdel synced f
            assert_equal 1 [r exists synced]
            assert_equal 1 [synced r synced]

            r exhmsetlww plain f v 1
            r exhdel plain f
            assert_equal 0 [r exists plain]
        }

        test {EXHSYNCGET with fields returns only those that exist, in request order} {
            r del k
            r exhsync k [r exhsyncbegin k] f1 a 10 f2 b 20
            assert_equal {1 {f2 b 20 f1 a 10}} [r exhsyncget k f2 missing f1]
        }

        test {EXHSYNCGET omits expired fields} {
            r del k
            r exhsync k [r exhsyncbegin k] f1 a 10
            r exhset k f2 v px 50
            after 120
            assert_equal {{f1 a 10}} [sync_entries [r exhsyncget k]]
            assert_equal {f1 a 10} [lindex [r exhsyncget k f1 f2] 1]
        }

        test {EXHSYNC rejects malformed input} {
            r del k
            assert_error {*wrong number of arguments*} {r exhsync k}
            assert_error {*wrong number of arguments*} {r exhsync k 1 f v}
            assert_error {*wrong number of arguments*} {r exhsyncbegin}
            assert_error {*wrong number of arguments*} {r exhsyncbegin k k}
            assert_error {*syntax*} {r exhsync k 0}
            assert_error {*syntax*} {r exhsync k -1}
            assert_error {*syntax*} {r exhsync k nope}
            set token [r exhsyncbegin k]
            assert_error {*syntax*} {r exhsync k $token f v 0}
            assert_equal 0 [synced r k]

            r set str x
            assert_error {*WRONGTYPE*} {r exhsync str 1}
            assert_error {*WRONGTYPE*} {r exhsyncget str}
            assert_error {*WRONGTYPE*} {r exhsyncbegin str}
        }

        test {EXHSYNCGET leaves watched keys alone} {
            r del k
            r exhsync k [r exhsyncbegin k] f a 10
            r watch k
            r exhsyncget k
            r exhsyncget k f
            r multi
            r ping
            assert_equal PONG [r exec]
        }

        test {DEBUG RELOAD makes synced hashes misses and keeps their data} {
            r flushall
            r exhsync synced [r exhsyncbegin synced] f1 a 10 f2 b 20
            r exhsync empty [r exhsyncbegin empty]
            r debug reload
            assert_equal {0 {}} [r exhsyncget synced]
            assert_equal {0 {}} [r exhsyncget empty]
            assert_equal {a 10} [r exhgetwithver synced f1]
            # A new sync makes it a hit again.
            r exhsync synced [r exhsyncbegin synced] f1 a 10 f2 b 20
            assert_equal {{f1 a 10} {f2 b 20}} [sync_entries [r exhsyncget synced]]
        }

        test {Sync state survives AOF replay and AOF rewrite as a miss, placeholders with their TTL} {
            r flushall
            r config set aof-use-rdb-preamble no
            r config set appendonly yes
            waitForBgrewriteaof r
            assert_equal ok [s aof_last_bgrewrite_status]

            # Replayed from the incremental AOF, i.e. the propagated effects.
            r exhsync synced [r exhsyncbegin synced] f1 a 10 gone x 1
            r exhsync synced [r exhsyncbegin synced] f1 a 10
            r exhsyncbegin placeholder
            set check {
                assert_equal {0 {}} [r exhsyncget synced]
                assert_equal {f1} [r exhkeys synced]
                assert {[r pttl placeholder] > 0}
            }
            r debug loadaof
            eval $check
            set digest [r debug digest]

            # Replayed from the rewritten base, i.e. TairHashTypeAofRewrite.
            r bgrewriteaof
            waitForBgrewriteaof r
            assert_equal ok [s aof_last_bgrewrite_status]
            r debug loadaof
            assert_equal $digest [r debug digest]
            eval $check
            r exhset placeholder g v
            assert_equal -1 [r ttl placeholder]

            r config set appendonly no
            r config set aof-use-rdb-preamble yes
        }
    }
}

start_server {tags {"tairhash lww retention"} overrides {bind 0.0.0.0}} {
    r module load $testmodule lww_tombstone_ttl 1 sync_lease_ttl 1

    test {A tombstone holds writes off until lww_tombstone_ttl} {
        r del k gone
        r exhmsetlww k keep v 1 f v 1
        r exhdellww k f 5
        r exhdellww gone f 5
        assert_equal {0} [r exhmsetlww k f old 4]
        after 1200
        assert_equal 0 [r exists gone]
        assert_equal {1} [r exhmsetlww k f old 4]
    }

    if {[tairhash_server_at_least 6 0]} {
        test {A sync lease ends after sync_lease_ttl} {
            r del placeholder k
            set token [r exhsyncbegin placeholder]
            r exhmsetlww k f v 1
            set k_token [r exhsyncbegin k]
            after 1200
            assert_equal 0 [r exists placeholder]
            assert_error {*EXHSYNCBEGIN*} {r exhsync k $k_token f v 1}
            set fresh [r exhsyncbegin k]
            assert {$fresh > $k_token}
            assert_equal {0 0} [r exhsync k $fresh f v 1]
        }

        test {A hash whose sync lease ended is not kept once it loses its last field} {
            r del k
            r exhmsetlww k f v 1
            r exhsyncbegin k
            after 1200
            r exhdel k f
            assert_equal 0 [r exists k]
        }
    }

    test {Expired tombstones are dropped as deletes go on} {
        r del k
        r exhmsetlww k keep v 1
        for {set i 0} {$i < 100} {incr i} {
            r exhdellww k "a$i" 1
        }
        set before [r memory usage k]
        after 1200
        # Enough deletes to double the count since the last purge, which drops the expired ones.
        for {set i 0} {$i < 30} {incr i} {
            r exhdellww k "b$i" 1
        }
        assert {[r memory usage k] < $before}
    }
}

# restart_server arrived with the Redis 6.2 test harness.
if {[info commands restart_server] ne {}} {
    start_server [list tags {"tairhash lww restart"} overrides [list bind 0.0.0.0 loadmodule $testmodule]] {
        test {A restart makes synced hashes misses and sequences continue above persisted ones} {
            r flushall
            r exhsync k [r exhsyncbegin k] f v 1
            r exhmsetlww k g v 1
            set token [r exhsyncbegin k]
            r save
            restart_server 0 true false
            assert_equal 0 [synced r k]
            assert_equal {v 1} [r exhgetwithver k f]
            assert_error {*EXHSYNCBEGIN*} {r exhsync k $token f v 1}
            assert {[r exhsyncbegin k] > $token}
        }
    }
}

# Last-write-wins writes carry their write sequence, which only the state format can, so
# they replicate in it even when replication_format is compat.
foreach format {state compat} {
    start_server {tags {"tairhash lww repl"} overrides {bind 0.0.0.0}} {
        r module load $testmodule
        set replica [srv 0 client]

        start_server {overrides {bind 0.0.0.0}} {
            r module load $testmodule replication_format $format
            set master [srv 0 client]

            set sync [tairhash_server_at_least 6 0]
            # Written before the replica attaches, so it arrives in the RDB of the full sync.
            $master exhmsetlww before f v 1
            $master exhdellww before g 3
            if {$sync} {
                $master exhsync before [$master exhsyncbegin before] f v 1
            }

            $replica replicaof [srv 0 host] [srv 0 port]
            wait_for_condition 50 100 {
                [lindex [$replica role] 0] eq {slave} &&
                [string match {*master_link_status:up*} [$replica info replication]]
            } else {
                fail "Can't turn the instance into a replica"
            }

            test "A full sync carries the LWW state to the replica ($format)" {
                wait_for_ofs_sync $master $replica
                assert_equal [$master debug digest] [$replica debug digest]
                if {$sync} {
                    assert_equal {1 {f v 1}} [$replica exhsyncget before]
                }
            }

            test "Replicas converge on LWW writes and deletes ($format)" {
                $master exhmsetlww w f1 a 10 f2 b 20
                $master exhmsetlww w f1 stale 5
                $master exhdellww w f2 20
                $master exhmsetlww w f2 old 19
                $master exhdellww wgone f 4
                wait_for_ofs_sync $master $replica
                assert_equal [$master debug digest] [$replica debug digest]
                assert_equal {f1} [$replica exhkeys w]
                assert {[$replica pttl wgone] > 0}
            }

            if {$sync} {
                test "Replicas converge on syncs ($format)" {
                    $master exhmsetlww k f1 a 10 f2 b 20 f3 c 1
                    $master exhmsetlww k f1 stale 5
                    set token [$master exhsyncbegin k]
                    $master exhmsetlww k late x 1
                    $master exhdellww k f3 2
                    $master exhsync k $token f1 a 10 f2 b 25 f3 c 1
                    $master exhsync k [$master exhsyncbegin k] f1 a 10 f2 b 25
                    $master exhsync empty [$master exhsyncbegin empty]
                    $master exhdellww gone f 4
                    $master exhsyncbegin placeholder

                    wait_for_ofs_sync $master $replica
                    assert_equal [$master debug digest] [$replica debug digest]
                    assert_equal {{f1 a 10} {f2 b 25}} [sync_entries [$replica exhsyncget k]]
                    assert_equal {1 {}} [$replica exhsyncget empty]
                    assert {[$replica pttl gone] > 0}
                    assert {[$replica pttl placeholder] > 0}
                    assert_equal -1 [$replica ttl k]
                }

                test "A promoted replica keeps the data and tombstones but nothing is synced ($format)" {
                    set token [$master exhsyncbegin k]
                    $replica replicaof no one
                    wait_for_condition 50 100 {
                        [lindex [$replica role] 0] eq {master}
                    } else {
                        fail "The replica was not promoted"
                    }
                    assert_equal {0 {}} [$replica exhsyncget k]
                    assert_equal {a 10} [$replica exhgetwithver k f1]
                    assert_equal {0} [$replica exhmsetlww gone f old 4]
                    assert_equal {0} [$replica exhmsetlww before g old 3]
                    # The old master's lease did not come along.
                    assert_error {*EXHSYNCBEGIN*} {$replica exhsync k $token f1 a 10}
                    $replica exhsync k [$replica exhsyncbegin k] f1 a 10 f2 b 25
                    assert_equal 1 [synced $replica k]
                }
            }
        }
    }
}
