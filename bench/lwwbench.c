/*
 * lwwbench: a pipelined load generator for the "current value cache" shape: many keys
 * (one per entity), a few dozen fields (channels) each, short values and
 * microsecond-timestamp versions.
 *
 *   lwwbench -w <workload> [-h host] [-p port] [-k keys] [-f fields] [-n ops]
 *            [-P pipeline] [-v value_len] [-s stale_pct] [-S seed]
 *
 * Population workloads write every field of every key once, in key order:
 *   load        EXHSYNC with a full snapshot (keys end up synced)
 *   loadset     EXHSET ... ABS <version> per field (also runs on modules without EXHSYNC)
 *   loadhset    native HSET with an 8-byte version prefix on each value: the memory
 *               baseline for the same data in a plain Redis hash
 *
 * Steady-state workloads pick a random key (and field) per op:
 *   lww         EXHMSETLWW of one field; `stale_pct`% of ops carry an older version
 *   gt          EXHSET ... GT, the request shape EXHMSETLWW replaces (stale = error reply)
 *   syncget     EXHSYNCGET of a whole key
 *   getallver   EXHGETALLWITHVER of a whole key, the read EXHSYNCGET replaces
 *   sync        EXHSYNCGET then EXHSYNC with a full snapshot (the read-repair path)
 *
 * Prints throughput and the number of error replies; error replies are counted, not fatal.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/time.h>
#include <unistd.h>

#include "hiredis.h"

#define MAX_FIELDS 256
#define MAX_VALUE_LEN 63
#define MAX_ARGS (3 + 3 * MAX_FIELDS)

typedef struct {
    const char *host;
    int port;
    const char *workload;
    long keys;
    long fields;
    long ops;
    long pipeline;
    long value_len;
    long stale_pct;
    unsigned seed;
} Config;

/* Backing storage for one command's arguments. */
typedef struct {
    char key[64];
    char token[24];
    char field[MAX_FIELDS][24];
    char value[MAX_FIELDS][8 + MAX_VALUE_LEN + 1];
    char version[MAX_FIELDS][24];
    const char *argv[MAX_ARGS];
    size_t argvlen[MAX_ARGS];
    int argc;
} Command;

static long long now_us(void) {
    struct timeval tv;
    gettimeofday(&tv, NULL);
    return (long long)tv.tv_sec * 1000000 + tv.tv_usec;
}

static void usage(void) {
    fprintf(stderr,
            "usage: lwwbench -w load|loadset|loadhset|lww|gt|syncget|getallver|sync [-h host] [-p port]\n"
            "                [-k keys] [-f fields] [-n ops] [-P pipeline] [-v value_len] [-s stale_pct] [-S seed]\n");
    exit(2);
}

static void arg(Command *cmd, const char *s, size_t len) {
    cmd->argv[cmd->argc] = s;
    cmd->argvlen[cmd->argc] = len;
    cmd->argc++;
}

static void arg_str(Command *cmd, const char *s) {
    arg(cmd, s, strlen(s));
}

static void set_value(char *buf, long len, long seed) {
    for (long i = 0; i < len; i++) {
        buf[i] = (char)('0' + (seed + i) % 10);
    }
}

static void set_key(Command *cmd, long k) {
    snprintf(cmd->key, sizeof(cmd->key), "data_cur_active:%ld:%ld", k % 97, k);
}

/* One field per key per call: <field> <value> <version> triples into slot i. */
static void set_field(Command *cmd, int i, long f, long value_seed, long len, long long version) {
    snprintf(cmd->field[i], sizeof(cmd->field[i]), "channel_%02ld", f);
    set_value(cmd->value[i], len, value_seed);
    snprintf(cmd->version[i], sizeof(cmd->version[i]), "%lld", version);
}

/* Builds a full-snapshot command for key k: EXHSYNC, or HSET with packed values. */
static void build_snapshot(Command *cmd, const Config *cfg, const char *name, long k, long long version, int packed) {
    cmd->argc = 0;
    set_key(cmd, k);
    arg_str(cmd, name);
    arg_str(cmd, cmd->key);
    if (!strcmp(name, "EXHSYNC")) {
        /* Token 0: nothing written before the snapshot may be removed. Every snapshot
         * carries all fields, so this exercises the same merge path as read-repair. */
        snprintf(cmd->token, sizeof(cmd->token), "0");
        arg_str(cmd, cmd->token);
    }
    for (long f = 0; f < cfg->fields; f++) {
        set_field(cmd, (int)f, f, k + f, cfg->value_len, version);
        arg_str(cmd, cmd->field[f]);
        if (packed) {
            memmove(cmd->value[f] + 8, cmd->value[f], cfg->value_len);
            memcpy(cmd->value[f], &version, 8);
            arg(cmd, cmd->value[f], 8 + cfg->value_len);
        } else {
            arg(cmd, cmd->value[f], cfg->value_len);
            arg_str(cmd, cmd->version[f]);
        }
    }
}

/* Appends the commands for operation `op`; returns how many replies to expect. */
static int append_op(redisContext *c, const Config *cfg, Command *cmd, long op, long long base_version) {
    const char *w = cfg->workload;
    long long version = base_version + op;

    if (!strcmp(w, "load") || !strcmp(w, "loadhset")) {
        build_snapshot(cmd, cfg, strcmp(w, "load") ? "HSET" : "EXHSYNC", op, version, !strcmp(w, "loadhset"));
        redisAppendCommandArgv(c, cmd->argc, cmd->argv, cmd->argvlen);
        return 1;
    }
    if (!strcmp(w, "loadset")) {
        long k = op / cfg->fields, f = op % cfg->fields;
        cmd->argc = 0;
        set_key(cmd, k);
        set_field(cmd, 0, f, k + f, cfg->value_len, version);
        arg_str(cmd, "EXHSET");
        arg_str(cmd, cmd->key);
        arg_str(cmd, cmd->field[0]);
        arg(cmd, cmd->value[0], cfg->value_len);
        arg_str(cmd, "ABS");
        arg_str(cmd, cmd->version[0]);
        redisAppendCommandArgv(c, cmd->argc, cmd->argv, cmd->argvlen);
        return 1;
    }

    long k = random() % cfg->keys;
    if (!strcmp(w, "sync")) {
        set_key(cmd, k);
        redisAppendCommand(c, "EXHSYNCGET %s", cmd->key);
        build_snapshot(cmd, cfg, "EXHSYNC", k, version, 0);
        redisAppendCommandArgv(c, cmd->argc, cmd->argv, cmd->argvlen);
        return 2;
    }

    cmd->argc = 0;
    set_key(cmd, k);
    if (!strcmp(w, "syncget") || !strcmp(w, "getallver")) {
        arg_str(cmd, !strcmp(w, "syncget") ? "EXHSYNCGET" : "EXHGETALLWITHVER");
        arg_str(cmd, cmd->key);
    } else if (!strcmp(w, "lww") || !strcmp(w, "gt")) {
        if (cfg->stale_pct > 0 && random() % 100 < cfg->stale_pct) {
            version = base_version - 1 - random() % 1000000;
        }
        set_field(cmd, 0, random() % cfg->fields, op, cfg->value_len, version);
        int lww = !strcmp(w, "lww");
        arg_str(cmd, lww ? "EXHMSETLWW" : "EXHSET");
        arg_str(cmd, cmd->key);
        arg_str(cmd, cmd->field[0]);
        arg(cmd, cmd->value[0], cfg->value_len);
        if (!lww) {
            arg_str(cmd, "GT");
        }
        arg_str(cmd, cmd->version[0]);
    } else {
        usage();
    }
    redisAppendCommandArgv(c, cmd->argc, cmd->argv, cmd->argvlen);
    return 1;
}

int main(int argc, char **argv) {
    Config cfg = {"127.0.0.1", 6379, NULL, 100000, 30, 1000000, 64, 8, 0, 1};
    int opt;
    while ((opt = getopt(argc, argv, "h:p:w:k:f:n:P:v:s:S:")) != -1) {
        switch (opt) {
            case 'h': cfg.host = optarg; break;
            case 'p': cfg.port = atoi(optarg); break;
            case 'w': cfg.workload = optarg; break;
            case 'k': cfg.keys = atol(optarg); break;
            case 'f': cfg.fields = atol(optarg); break;
            case 'n': cfg.ops = atol(optarg); break;
            case 'P': cfg.pipeline = atol(optarg); break;
            case 'v': cfg.value_len = atol(optarg); break;
            case 's': cfg.stale_pct = atol(optarg); break;
            case 'S': cfg.seed = (unsigned)atol(optarg); break;
            default: usage();
        }
    }
    if (cfg.workload == NULL || cfg.keys <= 0 || cfg.fields <= 0 || cfg.fields > MAX_FIELDS || cfg.value_len <= 0 ||
        cfg.value_len > MAX_VALUE_LEN || cfg.pipeline <= 0) {
        usage();
    }
    if (!strcmp(cfg.workload, "load") || !strcmp(cfg.workload, "loadhset")) {
        cfg.ops = cfg.keys;
    } else if (!strcmp(cfg.workload, "loadset")) {
        cfg.ops = cfg.keys * cfg.fields;
    }
    srandom(cfg.seed);

    redisContext *c = redisConnect(cfg.host, cfg.port);
    if (c == NULL || c->err) {
        fprintf(stderr, "connect %s:%d: %s\n", cfg.host, cfg.port, c ? c->errstr : "out of memory");
        return 1;
    }

    Command *cmd = calloc(1, sizeof(*cmd));
    long long base_version = now_us();
    long long errors = 0, replies = 0;
    long long start = now_us();

    for (long op = 0; op < cfg.ops;) {
        int pending = 0;
        for (long i = 0; i < cfg.pipeline && op < cfg.ops; i++, op++) {
            pending += append_op(c, &cfg, cmd, op, base_version);
        }
        for (int i = 0; i < pending; i++) {
            redisReply *reply;
            if (redisGetReply(c, (void **)&reply) != REDIS_OK) {
                fprintf(stderr, "reply: %s\n", c->errstr);
                return 1;
            }
            if (reply->type == REDIS_REPLY_ERROR) {
                if (errors == 0) {
                    fprintf(stderr, "first error reply: %s\n", reply->str);
                }
                errors++;
            }
            replies++;
            freeReplyObject(reply);
        }
    }

    double secs = (now_us() - start) / 1e6;
    printf("workload=%s keys=%ld fields=%ld ops=%ld pipeline=%ld value_len=%ld stale_pct=%ld secs=%.3f ops_per_sec=%.0f errors=%lld replies=%lld\n",
           cfg.workload, cfg.keys, cfg.fields, cfg.ops, cfg.pipeline, cfg.value_len, cfg.stale_pct, secs, cfg.ops / secs, errors, replies);
    free(cmd);
    redisFree(c);
    return 0;
}
