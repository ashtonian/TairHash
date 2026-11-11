set testmodule [file normalize your_path/tairhash_module.so]

# Major and minor version of the server under test (Valkey reports a compatible redis_version).
proc tairhash_server_version {} {
    lrange [split [s redis_version] .] 0 1
}

proc tairhash_server_at_least {major minor} {
    lassign [tairhash_server_version] ma mi
    expr {$ma > $major || ($ma == $major && $mi >= $minor)}
}

start_server {tags {"tairhash replication"} overrides {bind 0.0.0.0}} {
    r module load $testmodule

    test {MODULE LIST reports the module version} {
        set version {}
        foreach m [r module list] {
            if {[dict get $m name] eq {tairhash}} {
                set version [dict get $m ver]
            }
        }
        assert_equal 20000 $version
    }

    test {EXHSETNX starts a new field at version 1} {
        r del k
        assert_equal 1 [r exhsetnx k f v]
        assert_equal 1 [r exhver k f]
    }

    if {[tairhash_server_at_least 6 2]} {
        test {EXHMSETREPL is rejected from clients} {
            r del k
            assert_error {*replication stream*} {r exhmsetrepl k 1 f v 1 0}
            assert_equal 0 [r exists k]
        }
    }

    if {[tairhash_server_at_least 7 0]} {
        test {replication_format is a module config} {
            assert_equal {tairhash.replication_format state} [r config get tairhash.replication_format]
            r config set tairhash.replication_format compat
            assert_equal {tairhash.replication_format compat} [r config get tairhash.replication_format]
            assert_error {*} {r config set tairhash.replication_format bogus}
            r config set tairhash.replication_format state
        }
    }
}

foreach format {state compat} {
    start_server [list tags {"tairhash replication"} overrides {bind 0.0.0.0}] {
        r module load $testmodule replication_format $format

        test "Versions and TTLs survive DEBUG RELOAD ($format)" {
            r flushall
            r exhset ver f v abs 7
            r exhset ttl f v ex 1000
            set digest [r debug digest]
            r debug reload
            assert_equal $digest [r debug digest]
        }

        test "Versions and TTLs survive AOF replay and AOF rewrite ($format)" {
            r flushall
            r config set aof-use-rdb-preamble no
            r config set appendonly yes
            waitForBgrewriteaof r
            assert_equal ok [s aof_last_bgrewrite_status]

            # Replayed from the incremental AOF, i.e. the propagated effects.
            r exhsetnx nx f v
            r exhset ttl f v ex 1000
            r exhset ttl f v2 keepttl
            r exhincrby ttl n 1 ex 1000
            r exhset ttl p v ex 1000
            r exhpersist ttl p
            r exhsetver ttl f 42
            # More than one rewrite command's worth of fields in the state format.
            for {set i 0} {$i < 150} {incr i} {
                r exhset big f$i v$i
            }
            r exhset big ttl v px 100000
            set digest [r debug digest]
            r debug loadaof
            assert_equal $digest [r debug digest]

            # Replayed from the rewritten base, i.e. TairHashTypeAofRewrite.
            r bgrewriteaof
            waitForBgrewriteaof r
            assert_equal ok [s aof_last_bgrewrite_status]
            r debug loadaof
            assert_equal $digest [r debug digest]
            assert_equal 1 [r exhver nx f]
            assert {[r exhttl ttl f] > 0}
            assert_equal -1 [r exhttl ttl p]

            r config set appendonly no
            r config set aof-use-rdb-preamble yes
        }
    }

    start_server [list tags {"tairhash replication repl"} overrides {bind 0.0.0.0}] {
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

            test "Replicas converge on the versions and TTLs of every write command ($format)" {
                $master flushall
                $master exhset ttl keep v ex 1000
                $master exhset ttl keep v2 keepttl
                $master exhincrby ttl n 1 ex 1000
                $master exhincrbyfloat ttl x 1.5 px 100000
                $master exhset ttl p v ex 1000
                $master exhpersist ttl p
                $master exhsetnx ttl nx v
                $master exhmset ttl m1 v m2 v
                $master exhmset ttl new v m1 v2
                $master exhmsetwithopts ttl o v 0 1000
                $master exhsetver ttl m1 42

                wait_for_ofs_sync $master $replica
                assert_equal [$master debug digest] [$replica debug digest]
                assert_equal -1 [$replica exhttl ttl p]
                assert {[$replica exhttl ttl keep] > 0}
                assert {[$replica exhttl ttl n] <= 1000}
                assert_equal 1 [$replica exhver ttl nx]
            }
        }
    }
}
