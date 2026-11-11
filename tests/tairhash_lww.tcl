set testmodule [file normalize your_path/tairhash_module.so]

# Entries of an EXHSYNCGET reply as sorted {field value version} triples, so replies
# compare independently of hash iteration order.
proc sync_entries {reply} {
    set out {}
    foreach {f v ver} [lindex $reply 2] {
        lappend out [list $f $v $ver]
    }
    return [lsort -index 0 $out]
}

proc sync_token {client key} {
    lindex [$client exhsyncget $key] 1
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

    test {EXHSYNCGET reports a missing or never-synced hash as a miss with a token} {
        r del k
        set reply [r exhsyncget k]
        assert_equal 0 [lindex $reply 0]
        assert {[lindex $reply 1] >= 0}
        assert_equal {} [lindex $reply 2]

        r exhmsetlww k f v 1
        set reply [r exhsyncget k]
        assert_equal 0 [lindex $reply 0]
        assert_equal {} [lindex $reply 2]
        assert_equal 0 [lindex [r exhsyncget k f] 0]
    }

    test {EXHSYNCGET tokens never decrease and every later write sorts above them} {
        r del k
        set t1 [sync_token r k]
        r exhmsetlww k f v 1
        set t2 [sync_token r k]
        assert {$t2 > $t1}
        assert_equal $t2 [sync_token r k]
    }

    test {EXHSYNC merges the snapshot and marks the hash synced} {
        r del k
        assert_equal {2 0} [r exhsync k [sync_token r k] f1 a 10 f2 b 20]
        set reply [r exhsyncget k]
        assert_equal 1 [lindex $reply 0]
        assert_equal {{f1 a 10} {f2 b 20}} [sync_entries $reply]
    }

    test {EXHSYNC keeps newer cached writes and removes fields the source no longer has} {
        r del k
        r exhsync k [sync_token r k] f1 a 10 f2 b 20 gone x 5
        # The source now holds f2 at an older version than the cache, and no 'gone'.
        assert_equal {0 1} [r exhsync k [sync_token r k] f1 a 10 f2 old 15]
        assert_equal {{f1 a 10} {f2 b 20}} [sync_entries [r exhsyncget k]]
    }

    test {EXHSYNC keeps a field written after the token even when its version is old} {
        r del k
        r exhsync k [sync_token r k] f1 a 10
        set token [sync_token r k]
        # Raced the snapshot read: a late-arriving write carrying an old version.
        r exhmsetlww k late v 1
        assert_equal {0 0} [r exhsync k $token f1 a 10]
        assert_equal {{f1 a 10} {late v 1}} [sync_entries [r exhsyncget k]]

        # A sync whose token postdates that write removes it if the source lacks it.
        assert_equal {0 1} [r exhsync k [sync_token r k] f1 a 10]
        assert_equal {{f1 a 10}} [sync_entries [r exhsyncget k]]
    }

    test {EXHSYNC fencing holds across deleting and recreating the key} {
        r del k
        r exhsync k [sync_token r k] f1 a 10
        set token [sync_token r k]
        r del k
        r exhmsetlww k new v 1
        r exhsync k $token f1 a 10
        assert_equal {{f1 a 10} {new v 1}} [sync_entries [r exhsyncget k]]
    }

    test {EXHSYNC fencing covers every write command} {
        r del k
        r exhsync k [sync_token r k] base v 1
        set token [sync_token r k]
        r exhset k a v
        r exhsetnx k b v
        r exhmset k c v
        r exhmsetwithopts k d v 0 100
        r exhincrby k e 1
        r exhincrbyfloat k f 1.5
        assert_equal {0 0} [r exhsync k $token base v 1]
        assert_equal 7 [r exhlen k]
    }

    test {EXHSYNC with an empty snapshot caches an empty synced hash} {
        r del k
        assert_equal {0 0} [r exhsync k [sync_token r k]]
        assert_equal 1 [r exists k]
        assert_equal 0 [r exhlen k]
        set reply [r exhsyncget k]
        assert_equal 1 [lindex $reply 0]
        assert_equal {} [lindex $reply 2]
    }

    test {A synced hash survives losing its last field, an unsynced one does not} {
        r del synced plain
        r exhsync synced [sync_token r synced] f v 1
        r exhdel synced f
        assert_equal 1 [r exists synced]
        assert_equal 1 [lindex [r exhsyncget synced] 0]

        r exhmsetlww plain f v 1
        r exhdel plain f
        assert_equal 0 [r exists plain]
    }

    test {EXHSYNCGET with fields returns only those that exist, in request order} {
        r del k
        r exhsync k [sync_token r k] f1 a 10 f2 b 20
        set reply [r exhsyncget k f2 missing f1]
        assert_equal 1 [lindex $reply 0]
        assert_equal {f2 b 20 f1 a 10} [lindex $reply 2]
    }

    test {EXHSYNCGET omits expired fields} {
        r del k
        r exhsync k [sync_token r k] f1 a 10
        r exhset k f2 v px 50
        after 120
        assert_equal {{f1 a 10}} [sync_entries [r exhsyncget k]]
        assert_equal {f1 a 10} [lindex [r exhsyncget k f1 f2] 2]
    }

    test {EXHSYNC rejects malformed input} {
        r del k
        assert_error {*wrong number of arguments*} {r exhsync k}
        assert_error {*wrong number of arguments*} {r exhsync k 1 f v}
        assert_error {*syntax*} {r exhsync k -1}
        assert_error {*syntax*} {r exhsync k nope}
        assert_error {*syntax*} {r exhsync k 1 f v 0}
        assert_equal 0 [r exists k]

        r set str x
        assert_error {*WRONGTYPE*} {r exhsync str 1}
        assert_error {*WRONGTYPE*} {r exhsyncget str}
    }

    if {[tairhash_server_at_least 6 2]} {
        test {EXHSYNCREPL is rejected from clients} {
            r del k
            assert_error {*replication stream*} {r exhsyncrepl k}
            assert_equal 0 [r exists k]
        }
    }

    test {Sync state and sequences survive DEBUG RELOAD} {
        r flushall
        r exhsync synced [sync_token r synced] f1 a 10 f2 b 20
        r exhsync empty [sync_token r empty]
        r exhmsetlww plain f v 3
        set digest [r debug digest]
        r debug reload
        assert_equal $digest [r debug digest]
        assert_equal 1 [lindex [r exhsyncget synced] 0]
        assert_equal 1 [lindex [r exhsyncget empty] 0]
        assert_equal 0 [lindex [r exhsyncget plain] 0]
        assert_equal {{f1 a 10} {f2 b 20}} [sync_entries [r exhsyncget synced]]
    }

    test {Sync state and sequences survive AOF replay and AOF rewrite} {
        r flushall
        r config set aof-use-rdb-preamble no
        r config set appendonly yes
        waitForBgrewriteaof r
        assert_equal ok [s aof_last_bgrewrite_status]

        # Replayed from the incremental AOF, i.e. the propagated effects.
        r exhsync synced [sync_token r synced] f1 a 10 gone x 1
        r exhsync synced [sync_token r synced] f1 a 10
        r exhsync empty [sync_token r empty]
        r exhmsetlww plain f v 3
        set digest [r debug digest]
        r debug loadaof
        assert_equal $digest [r debug digest]

        # Replayed from the rewritten base, i.e. TairHashTypeAofRewrite.
        r bgrewriteaof
        waitForBgrewriteaof r
        assert_equal ok [s aof_last_bgrewrite_status]
        r debug loadaof
        assert_equal $digest [r debug digest]
        assert_equal 1 [lindex [r exhsyncget empty] 0]
        assert_equal {{f1 a 10}} [sync_entries [r exhsyncget synced]]

        r config set appendonly no
        r config set aof-use-rdb-preamble yes
    }
}

# restart_server arrived with the Redis 6.2 test harness.
if {[info commands restart_server] ne {}} {
    start_server [list tags {"tairhash lww restart"} overrides [list bind 0.0.0.0 loadmodule $testmodule]] {
        test {Write sequences continue above persisted ones after a restart} {
            r flushall
            r exhmsetlww k f v 1
            set token [sync_token r k]
            r save
            restart_server 0 true false
            # The loaded data carries the sequence of the last write, so tokens resume there.
            assert_equal $token [sync_token r k]
            r exhmsetlww k g v 1
            assert {[sync_token r k] > $token}
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

            $replica replicaof [srv 0 host] [srv 0 port]
            wait_for_condition 50 100 {
                [lindex [$replica role] 0] eq {slave} &&
                [string match {*master_link_status:up*} [$replica info replication]]
            } else {
                fail "Can't turn the instance into a replica"
            }

            test "Replicas converge on LWW writes, syncs and write sequences ($format)" {
                $master flushall
                $master exhmsetlww k f1 a 10 f2 b 20
                $master exhmsetlww k f1 stale 5
                set token [sync_token $master k]
                $master exhmsetlww k late x 1
                $master exhsync k $token f1 a 10 f2 b 25
                $master exhsync k [sync_token $master k] f1 a 10 f2 b 25
                $master exhsync empty [sync_token $master empty]

                wait_for_ofs_sync $master $replica
                assert_equal [$master debug digest] [$replica debug digest]
                assert_equal [sync_entries [$master exhsyncget k]] [sync_entries [$replica exhsyncget k]]
                assert_equal {{f1 a 10} {f2 b 25}} [sync_entries [$replica exhsyncget k]]
                assert_equal 1 [lindex [$replica exhsyncget empty] 0]
                assert_equal [sync_token $master k] [sync_token $replica k]
            }

            test "A token from the old master still fences writes on a promoted replica ($format)" {
                set token [sync_token $master k]
                $replica replicaof no one
                $replica exhmsetlww k after v 1
                $replica exhsync k $token f1 a 10 f2 b 25
                assert_equal {{after v 1} {f1 a 10} {f2 b 25}} [sync_entries [$replica exhsyncget k]]
            }
        }
    }
}
