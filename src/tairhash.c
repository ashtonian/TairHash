/*
 * Copyright 2021 Alibaba Tair Team
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#include "tairhash.h"

#include <assert.h>
#include <ctype.h>
#include <errno.h>
#include <float.h>
#include <limits.h>
#include <math.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <sys/time.h>
#include <time.h>
#include <unistd.h>

#include "scan_algorithm.h"
#include "slab_algorithm.h"
#include "sort_algorithm.h"

RedisModuleType *TairHashType;

static int redis_major_ver = 0;
static int redis_minor_ver = 0;
static int redis_patch_ver = 0;

#if defined(SORT_MODE) || defined(SLAB_MODE)
m_zskiplist *g_expire_index[DB_NUM];
#endif

RedisModuleTimerID g_expire_timer_id;
ExpireAlgorithm g_expire_algorithm;

void _moduleAssert(const char *estr, const char *file, int line) {
    fprintf(stderr, "=== ASSERTION FAILED ===");
    fprintf(stderr, "==> %s:%d '%s' is not true", file, line, estr);
    *((char *)-1) = 'x';
}

struct TairHashVal *createTairHashVal(void) {
    struct TairHashVal *o;
    o = RedisModule_Calloc(1, sizeof(*o));
    return o;
}

void tairHashValRelease(struct TairHashVal *o) {
    if (o) {
        if (o->value) {
            RedisModule_FreeString(NULL, o->value);
        }
        RedisModule_Free(o);
    }
}

RedisModuleString *takeAndRef(RedisModuleString *str) {
    RedisModule_RetainString(NULL, str);
    return str;
}

int isExpire(long long when) {
    if (when == 0) return 0;
    return RedisModule_Milliseconds() > when;
}

int delEmptyTairHashIfNeeded(RedisModuleCtx *ctx, RedisModuleKey *key, RedisModuleString *raw_key, tairHashObj *obj) {
    if (!obj || obj->synced || (RedisModule_GetContextFlags(ctx) & REDISMODULE_CTX_FLAGS_SLAVE) || (dictSize(obj->hash) != 0)) {
        return 0;
    }

    if (key == NULL) {
        key = RedisModule_OpenKey(ctx, raw_key, REDISMODULE_WRITE);
        if (RedisModule_KeyType(key) == REDISMODULE_KEYTYPE_EMPTY) {
            RedisModule_CloseKey(key);
            return 0;
        }
    }

    if (redis_major_ver < 6 || (redis_major_ver == 6 && redis_minor_ver < 2)) {
        /* See bugfix: https://github.com/redis/redis/pull/8617
                       https://github.com/redis/redis/pull/8097
                       https://github.com/redis/redis/pull/7037
        */
        RedisModule_CloseKey(key);
        RedisModuleCtx *ctx2 = RedisModule_GetThreadSafeContext(NULL);
        RedisModule_SelectDb(ctx2, RedisModule_GetSelectedDb(ctx));
        RedisModuleCallReply *reply = RedisModule_Call(ctx2, "DEL", "s!", raw_key);
        if (reply != NULL) RedisModule_FreeCallReply(reply);
        RedisModule_FreeThreadSafeContext(ctx2);
    } else {
        RedisModule_DeleteKey(key);
        RedisModule_Replicate(ctx, "DEL", "s", raw_key);
        if (RedisModule_NotifyKeyspaceEvent) {
            RedisModule_NotifyKeyspaceEvent(ctx, REDISMODULE_NOTIFY_GENERIC, "del", raw_key);
        }
        RedisModule_CloseKey(key);
    }
    return 1;
}

void notifyFieldSpaceEvent(char *event, RedisModuleString *key, RedisModuleString *field, int dbid) {
    size_t key_len, field_len;
    const char *key_ptr = RedisModule_StringPtrLen(key, &key_len);
    const char *field_ptr = RedisModule_StringPtrLen(field, &field_len);
    /* tairhash@<db>@<key>__:<event> <field> notifications. */
    RedisModuleString *channel = RedisModule_CreateStringPrintf(NULL, "tairhash@%d@%s__:%s", dbid, key_ptr, event);
    RedisModuleString *message = RedisModule_CreateStringFromString(NULL, field);

    if (RedisModule_PublishMessage) {
        RedisModule_PublishMessage(NULL, channel, message);
    } else {
        RedisModuleCtx *ctx = RedisModule_GetThreadSafeContext(NULL);
        RedisModule_SelectDb(ctx, dbid);
        RedisModuleCallReply *reply = RedisModule_Call(ctx, "PUBLISH", "ss", channel, message);
        if (reply != NULL) RedisModule_FreeCallReply(reply);
        RedisModule_FreeThreadSafeContext(ctx);
    }

    RedisModule_FreeString(NULL, channel);
    RedisModule_FreeString(NULL, message);
}

void tairhashScanCallback(void *privdata, const m_dictEntry *de) {
    list *keys = (list *)privdata;
    RedisModuleString *key, *val = NULL;

    RedisModuleString *skey = dictGetKey(de);
    TairHashVal *sval = dictGetVal(de);
    key = skey;
    if (sval) {
        val = sval->value;
    }
    m_listAddNodeTail(keys, key);
    if (val) {
        m_listAddNodeTail(keys, val);
    }
}

uint64_t dictModuleStrHash(const void *key) {
    size_t len;
    const char *buf = RedisModule_StringPtrLen(key, &len);
    return m_dictGenHashFunction(buf, (int)len);
}

int dictModuleStrKeyCompare(void *privdata, const void *key1,
                            const void *key2) {
    size_t l1, l2;
    DICT_NOTUSED(privdata);

    const char *buf1 = RedisModule_StringPtrLen(key1, &l1);
    const char *buf2 = RedisModule_StringPtrLen(key2, &l2);
    if (l1 != l2) return 0;
    return memcmp(buf1, buf2, l1) == 0;
}

void dictModuleKeyDestructor(void *privdata, void *val) {
    DICT_NOTUSED(privdata);
    if (val) {
        RedisModule_FreeString(NULL, val);
    }
}

void dictModuleValueDestructor(void *privdata, void *val) {
    DICT_NOTUSED(privdata);
    if (val) {
        TairHashVal *v = (TairHashVal *)val;
        tairHashValRelease(v);
    }
}

m_dictType tairhashDictType = {
    dictModuleStrHash,        /* hash function */
    NULL,                     /* key dup */
    NULL,                     /* val dup */
    dictModuleStrKeyCompare,  /* key compare */
    dictModuleKeyDestructor,  /* key destructor */
    dictModuleValueDestructor /* val destructor */
};

static void tairHashTypeReleaseObject(struct tairHashObj *o) {
    m_dictRelease(o->hash);
    if (o->expire_index) {
#ifdef SLAB_MODE
        slab_free(o->expire_index);
#else
        m_zslFree(o->expire_index);
#endif
    }
    if (o->key) {
        RedisModule_FreeString(NULL, o->key);
    }
    RedisModule_Free(o);
}

static struct tairHashObj *createTairHashTypeObject() {
    tairHashObj *o = RedisModule_Calloc(1, sizeof(*o));
    o->hash = m_dictCreate(&tairhashDictType, NULL);
    return o;
}

void tairHashEnsureExpireIndex(tairHashObj *o) {
    if (o->expire_index) {
        return;
    }
#ifdef SLAB_MODE
    o->expire_index = slab_create();
#else
    o->expire_index = m_zslCreate();
#endif
}

/* ========================== Write sequence =============================*/

/* Highest write sequence assigned or observed (replication, RDB, AOF) by this server. */
static long long g_last_seq = 0;

/* Set at load time when the server provides RedisModule_CachedMicroseconds (Redis 7.2+). */
static long long (*g_cached_microseconds)(void) = NULL;

static long long ustime(void) {
    struct timeval tv;
    gettimeofday(&tv, NULL);
    return ((long long)tv.tv_sec) * 1000000 + tv.tv_usec;
}

/* Wall-clock microseconds for sequence assignment. Read it once per command and pass
 * it to every tairHashNextSeq of that command: a clock read per field was a measurable
 * share of a write. The server's cached command time is used where available. */
static long long seqClockNow(void) {
    return g_cached_microseconds ? g_cached_microseconds() : ustime();
}

/* A hybrid logical clock: max(last + 1, wall-clock microseconds). It is strictly
 * increasing within a process, and because it tracks wall time it stays above the
 * sequences a previous process (or a failed-over master) handed out, even when no
 * state was persisted. Sequences are replicated explicitly, never re-derived. */
static long long tairHashNextSeq(long long now) {
    g_last_seq = now > g_last_seq ? now : g_last_seq + 1;
    return g_last_seq;
}

static void tairHashObserveSeq(long long seq) {
    if (seq > g_last_seq) {
        g_last_seq = seq;
    }
}

/* Set at load time when the server reports the REPLICATED and LOADING context flags
 * (RedisModule_GetContextFlagsAll, Redis 6.2+). */
static int g_can_identify_restoring = 0;

/* Whether the caller may restore exact state (EXHMSETREPL, EXHSYNCREPL): the
 * replication stream or AOF loading carry state the master already decided, and a
 * regular client must not be able to forge it. A server that cannot report those
 * contexts cannot tell callers apart, so restoring stays allowed there. */
static int mayRestoreState(RedisModuleCtx *ctx) {
    if (!g_can_identify_restoring) {
        return 1;
    }
    int flags = RedisModule_GetContextFlags(ctx);
    return (flags & (REDISMODULE_CTX_FLAGS_REPLICATED | REDISMODULE_CTX_FLAGS_LOADING)) != 0;
}

/* Arguments per field of EXHMSETREPL: field, value, version, expire, seq. */
#define FIELD_STATE_ARGS 5

/* Shared "0" for the expire of fields without a TTL; created at load time. */
static RedisModuleString *g_zero_str = NULL;

/* Collects the resulting state of the fields one command writes and propagates it as a
 * single EXHMSETREPL. Propagating state rather than the request makes replicas and the
 * AOF converge regardless of their clock (relative TTLs), their own state (KEEPTTL,
 * GT/VER checks, version increments) and their write sequence; batching keeps that to
 * one command lookup and one argv per command instead of one per field. */
typedef struct {
    RedisModuleCtx *ctx;
    RedisModuleString **argv;
    int argc;
    int cap;
} FieldStateBatch;

static void fieldStateBatchInit(FieldStateBatch *b, RedisModuleCtx *ctx, RedisModuleString *key, int fields_hint) {
    b->ctx = ctx;
    b->cap = 1 + FIELD_STATE_ARGS * (fields_hint > 0 ? fields_hint : 1);
    b->argv = RedisModule_Alloc(sizeof(RedisModuleString *) * b->cap);
    b->argv[0] = key;
    b->argc = 1;
}

/* Snapshots the field's state. The batch holds its own references, so a later write to
 * the same field within the command (repeated fields) cannot invalidate it.
 * `version_str`, when not NULL, is the version already encoded as a decimal string (a
 * client argument), which saves converting it again. */
static void fieldStateBatchAdd(FieldStateBatch *b, RedisModuleString *field, const TairHashVal *val, RedisModuleString *version_str) {
    if (b->argc + FIELD_STATE_ARGS > b->cap) {
        b->cap *= 2;
        b->argv = RedisModule_Realloc(b->argv, sizeof(RedisModuleString *) * b->cap);
    }
    b->argv[b->argc++] = takeAndRef(field);
    b->argv[b->argc++] = takeAndRef(val->value);
    b->argv[b->argc++] = version_str ? takeAndRef(version_str) : RedisModule_CreateStringFromLongLong(NULL, val->version);
    b->argv[b->argc++] = val->expire ? RedisModule_CreateStringFromLongLong(NULL, val->expire) : g_zero_str;
    b->argv[b->argc++] = RedisModule_CreateStringFromLongLong(NULL, val->seq);
}

/* Propagates the collected state, if any, and releases the batch (argv[0], the key,
 * belongs to the caller). */
static void fieldStateBatchFlush(FieldStateBatch *b) {
    if (b->argc > 1) {
        RedisModule_Replicate(b->ctx, "EXHMSETREPL", "v", b->argv, (size_t)b->argc);
    }
    for (int i = 1; i < b->argc; i++) {
        if (b->argv[i] != g_zero_str) {
            RedisModule_FreeString(NULL, b->argv[i]);
        }
    }
    RedisModule_Free(b->argv);
    b->argv = NULL;
}

static void replicateFieldState(RedisModuleCtx *ctx, RedisModuleString *key, RedisModuleString *field, const TairHashVal *val) {
    FieldStateBatch batch;
    fieldStateBatchInit(&batch, ctx, key, 1);
    fieldStateBatchAdd(&batch, field, val, NULL);
    fieldStateBatchFlush(&batch);
}

int isReadOnlyStatus(RedisModuleCtx *ctx) {
    int flags = RedisModule_GetContextFlags(ctx);
    if (flags & REDISMODULE_CTX_FLAGS_SLAVE || flags & REDISMODULE_CTX_FLAGS_READONLY) {
        return 1;
    }

    return 0;
}

/* ========================== Common  func =============================*/
void activeExpireTimerHandler(RedisModuleCtx *ctx, void *data) {
    REDISMODULE_NOT_USED(data);
    RedisModule_AutoMemory(ctx);
    static uint64_t loop_cnt = 0, total_expire_time = 0;
    static unsigned int current_db = 0;
    int dbs_per_call = g_expire_algorithm.dbs_per_active_loop;

    if (isReadOnlyStatus(ctx)) {
        goto restart;
    }

    long long start = RedisModule_Milliseconds();

    for (int i = 0; i < dbs_per_call; ++i) {
        current_db = current_db % DB_NUM;
        if (RedisModule_SelectDb(ctx, current_db) != REDISMODULE_OK) {
            current_db++;
            continue;
        }

        if (RedisModule_DbSize && RedisModule_DbSize(ctx) == 0) {
            current_db++;
            continue;
        }

        /* Perform active expire algorithm. */
        g_expire_algorithm.activeExpire(ctx, current_db, g_expire_algorithm.keys_per_active_loop);
        current_db++;
    }

    g_expire_algorithm.stat_last_active_expire_time_msec = RedisModule_Milliseconds() - start;
    if (g_expire_algorithm.stat_max_active_expire_time_msec < g_expire_algorithm.stat_last_active_expire_time_msec) {
        g_expire_algorithm.stat_max_active_expire_time_msec = g_expire_algorithm.stat_last_active_expire_time_msec;
    }
    total_expire_time += g_expire_algorithm.stat_last_active_expire_time_msec;
    if (++loop_cnt % 10 == 0) {
        g_expire_algorithm.stat_avg_active_expire_time_msec = total_expire_time / loop_cnt;
        loop_cnt = 0;
        total_expire_time = 0;
    }

restart:
    if (g_expire_algorithm.enable_active_expire) {
        g_expire_timer_id = RedisModule_CreateTimer(ctx, g_expire_algorithm.active_expire_period, activeExpireTimerHandler, NULL);
    }
}

int fieldExpireIfNeeded(RedisModuleCtx *ctx, int dbid, RedisModuleString *key, tairHashObj *o, RedisModuleString *field, int is_timer) {
    TairHashVal *tair_hash_val = m_dictFetchValue(o->hash, field);
    if (tair_hash_val == NULL) {
        return 0;
    }

    long long when = tair_hash_val->expire;
    if (when == 0) {
        return 0;
    }

    long long now = RedisModule_Milliseconds();
    if (isReadOnlyStatus(ctx)) {
        return now > when;
    }

    if (now < when) {
        return 0;
    }

    g_expire_algorithm.deleteAndPropagate(ctx, dbid, key, o, field, when, is_timer);
    return 1;
}

#if defined(SORT_MODE) || defined(SLAB_MODE)
void swapDbCallback(RedisModuleCtx *ctx, RedisModuleEvent e, uint64_t sub, void *data) {
    REDISMODULE_NOT_USED(e);
    REDISMODULE_NOT_USED(sub);
    RedisModule_AutoMemory(ctx);

    RedisModuleSwapDbInfo *ei = data;

    int from_dbid = ei->dbnum_first;
    int to_dbid = ei->dbnum_second;

    /* 1. swap index */
    m_zskiplist *tmp_zsl = g_expire_index[from_dbid];
    g_expire_index[from_dbid] = g_expire_index[to_dbid];
    g_expire_index[to_dbid] = tmp_zsl;

    /* 2. swap statistics*/
    uint64_t tmp_stat = g_expire_algorithm.stat_active_expired_field[from_dbid];
    g_expire_algorithm.stat_active_expired_field[from_dbid] = g_expire_algorithm.stat_active_expired_field[to_dbid];
    g_expire_algorithm.stat_active_expired_field[to_dbid] = tmp_stat;

    tmp_stat = g_expire_algorithm.stat_passive_expired_field[from_dbid];
    g_expire_algorithm.stat_passive_expired_field[from_dbid] = g_expire_algorithm.stat_passive_expired_field[to_dbid];
    g_expire_algorithm.stat_passive_expired_field[to_dbid] = tmp_stat;
}

void flushDbCallback(RedisModuleCtx *ctx, RedisModuleEvent e, uint64_t sub, void *data) {
    REDISMODULE_NOT_USED(e);
    REDISMODULE_NOT_USED(sub);
    RedisModule_AutoMemory(ctx);

    RedisModuleFlushInfo *fi = data;
    if (sub == REDISMODULE_SUBEVENT_FLUSHDB_START) {
        if (fi->dbnum != -1) {
            /* Free and Re-Create index. */
            m_zslFree(g_expire_index[fi->dbnum]);
            g_expire_index[fi->dbnum] = m_zslCreate();
        } else {
            for (int i = 0; i < DB_NUM; i++) {
                m_zslFree(g_expire_index[i]);
                g_expire_index[i] = m_zslCreate();
            }
        }
    }
}

static int keySpaceNotification(RedisModuleCtx *ctx, int type, const char *event, RedisModuleString *key) {
    REDISMODULE_NOT_USED(ctx);
    REDISMODULE_NOT_USED(type);
    RedisModule_AutoMemory(ctx);

#define CMD_NONE 0
#define CMD_RENAME 1
#define CMD_MOVE 2

    static RedisModuleString *from_key = NULL, *to_key = NULL;
    static int from_dbid, to_dbid;
    static int cmd_flag = CMD_NONE;

    int dbid = RedisModule_GetSelectedDb(ctx);

    if (strcmp(event, "rename_from") == 0) {
        from_key = RedisModule_CreateStringFromString(NULL, key);
    } else if (strcmp(event, "rename_to") == 0) {
        to_key = RedisModule_CreateStringFromString(NULL, key);
        cmd_flag = CMD_RENAME;
    } else if (strcmp(event, "move_from") == 0) {
        from_dbid = dbid;
    } else if (strcmp(event, "move_to") == 0) {
        to_dbid = dbid;
        cmd_flag = CMD_MOVE;
    }

    if (cmd_flag != CMD_NONE) {
        RedisModuleString *local_from_key = NULL, *local_to_key = NULL;
        int local_from_dbid, local_to_dbid;
        /* We assign values in advance so that `move` and `rename` can be processed uniformly. */
        if (cmd_flag == CMD_RENAME) {
            local_from_key = from_key;
            local_to_key = to_key;
            /* `rename` does not change the dbid of the key. */
            local_from_dbid = dbid;
            local_to_dbid = dbid;
        } else {
            /* `move` does not change the name of the key. */
            local_from_key = key;
            local_to_key = key;
            local_from_dbid = from_dbid;
            local_to_dbid = to_dbid;
        }

        RedisModule_SelectDb(ctx, local_to_dbid);
        RedisModuleKey *real_key = RedisModule_OpenKey(ctx, local_to_key, REDISMODULE_READ | REDISMODULE_WRITE);
        tairHashObj *tair_hash_obj = NULL;
        if (RedisModule_KeyType(real_key) == REDISMODULE_KEYTYPE_MODULE && RedisModule_ModuleTypeGetType(real_key) == TairHashType) {
            tair_hash_obj = RedisModule_ModuleTypeGetValue(real_key);
        }

        /* Keys of every type are renamed and moved. Only a tairhash tracks its own name,
         * and only one with expiring fields has an entry in the db expire index. */
        if (tair_hash_obj) {
            int indexed = TAIRHASH_EXPIRE_INDEX_LEN(tair_hash_obj) > 0;
            long long previous_index = 0;
            if (indexed) {
#ifdef SLAB_MODE
                previous_index = tair_hash_obj->expire_index->header->level[0].forward->expire_min;
#else
                previous_index = tair_hash_obj->expire_index->header->level[0].forward->score;
#endif
                m_zslDelete(g_expire_index[local_from_dbid], previous_index, local_from_key, NULL);
            }
            if (tair_hash_obj->key) {
                RedisModule_FreeString(NULL, tair_hash_obj->key);
                tair_hash_obj->key = RedisModule_CreateStringFromString(NULL, local_to_key);
            }
            if (indexed) {
                m_zslInsert(g_expire_index[local_to_dbid], previous_index, takeAndRef(tair_hash_obj->key));
            }
        }

        /* Release sources. */
        if (cmd_flag == CMD_RENAME) {
            if (to_key) {
                RedisModule_FreeString(NULL, to_key);
                to_key = NULL;
            }

            if (from_key) {
                RedisModule_FreeString(NULL, from_key);
                from_key = NULL;
            }
        }

        /* Reset flag. */
        cmd_flag = CMD_NONE;
    }

    return REDISMODULE_OK;
}

void infoFunc(RedisModuleInfoCtx *ctx, int for_crash_report) {
    RedisModule_InfoAddSection(ctx, "Statistics");
    RedisModule_InfoAddFieldLongLong(ctx, "active_expire_enable", g_expire_algorithm.enable_active_expire);
    RedisModule_InfoAddFieldLongLong(ctx, "active_expire_period", g_expire_algorithm.active_expire_period);
    RedisModule_InfoAddFieldLongLong(ctx, "active_expire_keys_per_loop", g_expire_algorithm.keys_per_active_loop);
    RedisModule_InfoAddFieldLongLong(ctx, "active_expire_dbs_per_loop", g_expire_algorithm.dbs_per_active_loop);
    RedisModule_InfoAddFieldLongLong(ctx, "active_expire_last_time_msec", g_expire_algorithm.stat_last_active_expire_time_msec);
    RedisModule_InfoAddFieldLongLong(ctx, "active_expire_max_time_msec", g_expire_algorithm.stat_max_active_expire_time_msec);
    RedisModule_InfoAddFieldLongLong(ctx, "active_expire_avg_time_msec", g_expire_algorithm.stat_avg_active_expire_time_msec);
    RedisModule_InfoAddFieldLongLong(ctx, "passive_expire_keys_per_loop", g_expire_algorithm.keys_per_passive_loop);

    RedisModule_InfoAddSection(ctx, "ActiveExpiredFields");
    char buf[10];
    for (int i = 0; i < DB_NUM; ++i) {
        if (g_expire_index[i]->length == 0 && g_expire_algorithm.stat_active_expired_field[i] == 0) {
            continue;
        }
        snprintf(buf, sizeof(buf), "db%d", i);
        RedisModule_InfoAddFieldLongLong(ctx, buf, g_expire_algorithm.stat_active_expired_field[i]);
    }

    RedisModule_InfoAddSection(ctx, "PassiveExpiredFields");
    for (int i = 0; i < DB_NUM; ++i) {
        if (g_expire_index[i]->length == 0 && g_expire_algorithm.stat_passive_expired_field[i] == 0) {
            continue;
        }
        snprintf(buf, sizeof(buf), "db%d", i);
        RedisModule_InfoAddFieldLongLong(ctx, buf, g_expire_algorithm.stat_passive_expired_field[i]);
    }
}

#endif

void startExpireTimer(RedisModuleCtx *ctx, void *data) {
    if (!g_expire_algorithm.enable_active_expire) {
        return;
    }

    if (RedisModule_GetTimerInfo(ctx, g_expire_timer_id, NULL, NULL) == REDISMODULE_OK) {
        return;
    }

    g_expire_timer_id = RedisModule_CreateTimer(ctx, g_expire_algorithm.active_expire_period, activeExpireTimerHandler, data);
}

static int mstrcasecmp(const RedisModuleString *rs1, const char *s2) {
    size_t n1 = strlen(s2);
    size_t n2;
    const char *s1 = RedisModule_StringPtrLen(rs1, &n2);
    if (n1 != n2) {
        return -1;
    }
    return strncasecmp(s1, s2, n1);
}

static int mstrmatchlen(RedisModuleString *pattern, RedisModuleString *str, int nocase) {
    size_t plen, slen;
    const char *pattern_p = RedisModule_StringPtrLen(pattern, &plen);
    const char *str_p = RedisModule_StringPtrLen(str, &slen);
    return m_stringmatchlen(pattern_p, plen, str_p, slen, nocase);
}

int tairHashExpireGenericFunc(RedisModuleCtx *ctx, RedisModuleString **argv, int argc, long long basetime, int unit) {
    RedisModule_AutoMemory(ctx);

    if (argc < 4 || argc > 7) {
        return RedisModule_WrongArity(ctx);
    }

    long long milliseconds;
    long long version = 0;
    int field_expired = 0;
    int nokey;

    if (RedisModule_StringToLongLong(argv[3], &milliseconds) != REDISMODULE_OK) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    if (milliseconds < 0) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    RedisModuleString *version_p = NULL;
    int ex_flags = TAIR_HASH_SET_NO_FLAGS;

    if (argc > 4) {
        for (int j = 4; j < argc; j++) {
            RedisModuleString *next = (j == argc - 1) ? NULL : argv[j + 1];
            if (!mstrcasecmp(argv[4], "ver") && !(ex_flags & TAIR_HASH_SET_WITH_ABS_VER) && !(ex_flags & TAIR_HASH_SET_WITH_GT_VER) && next) {
                ex_flags |= TAIR_HASH_SET_WITH_VER;
                version_p = next;
                j++;
            } else if (!mstrcasecmp(argv[4], "abs") && !(ex_flags & TAIR_HASH_SET_WITH_VER) && !(ex_flags & TAIR_HASH_SET_WITH_GT_VER) && next) {
                ex_flags |= TAIR_HASH_SET_WITH_ABS_VER;
                version_p = next;
                j++;
            } else if (!mstrcasecmp(argv[4], "gt") && !(ex_flags & TAIR_HASH_SET_WITH_VER) && !(ex_flags & TAIR_HASH_SET_WITH_ABS_VER) && next) {
                ex_flags |= TAIR_HASH_SET_WITH_GT_VER;
                version_p = next;
                j++;
            } else {
                RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
                return REDISMODULE_ERR;
            }
        }
    }

    if ((NULL != version_p) && (RedisModule_StringToLongLong(version_p, &version) != REDISMODULE_OK)) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    if (version < 0 || ((ex_flags & (TAIR_HASH_SET_WITH_ABS_VER | TAIR_HASH_SET_WITH_GT_VER)) && version == 0)) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        RedisModule_ReplyWithLongLong(ctx, 0);
        return REDISMODULE_OK;
    } else {
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    RedisModuleString *skey = argv[2], *pkey = argv[1];

    int dbid = RedisModule_GetSelectedDb(ctx);
    if (fieldExpireIfNeeded(ctx, dbid, pkey, tair_hash_obj, skey, 0)) {
        field_expired = 1;
    }

    TairHashVal *tair_hash_val = NULL;
    m_dictEntry *de = m_dictFind(tair_hash_obj->hash, skey);
    if (field_expired || de == NULL) {
        nokey = 1;
        RedisModule_ReplyWithLongLong(ctx, 0);
    } else {
        nokey = 0;
        skey = dictGetKey(de);
        tair_hash_val = dictGetVal(de);
        if (ex_flags & TAIR_HASH_SET_WITH_VER) {
            if (version != 0 && version != tair_hash_val->version) {
                RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_VERSION);
                return REDISMODULE_ERR;
            }
        } else if (ex_flags & TAIR_HASH_SET_WITH_GT_VER) {
            if (version <= tair_hash_val->version) {
                RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_VERSION);
                return REDISMODULE_ERR;
            }
        }

        if (milliseconds == 0) {
            milliseconds = 1;
        } else {
            if (unit == UNIT_SECONDS) {
                milliseconds *= 1000;
            }
            milliseconds += basetime;
        }

        if (milliseconds > 0) {
            int dbid = RedisModule_GetSelectedDb(ctx);
            if (nokey || tair_hash_val->expire == 0) {
                g_expire_algorithm.insert(ctx, dbid, argv[1], tair_hash_obj, skey, milliseconds);
            } else {
                g_expire_algorithm.update(ctx, dbid, argv[1], tair_hash_obj, skey, tair_hash_val->expire, milliseconds);
            }
            tair_hash_val->expire = milliseconds;
        }

        RedisModule_ReplyWithLongLong(ctx, 1);

        if (ex_flags & (TAIR_HASH_SET_WITH_ABS_VER | TAIR_HASH_SET_WITH_GT_VER)) {
            tair_hash_val->version = version;
        } else {
            tair_hash_val->version += 1;
        }

        size_t vlen = 0, VSIZE_MAX = 5;
        RedisModuleString **v = RedisModule_Alloc(sizeof(RedisModuleString *) * VSIZE_MAX);
        v[vlen++] = RedisModule_CreateStringFromString(ctx, argv[1]);
        v[vlen++] = RedisModule_CreateStringFromString(ctx, argv[2]);
        v[vlen++] = RedisModule_CreateStringFromLongLong(ctx, tair_hash_val->expire);
        if (version_p) {
            v[vlen++] = RedisModule_CreateString(ctx, "ABS", 3);
            v[vlen++] = RedisModule_CreateStringFromLongLong(ctx, tair_hash_val->version);
        }

        RedisModule_Replicate(ctx, "EXHPEXPIREAT", "v", v, vlen);
        RedisModule_Free(v);
    }

    delEmptyTairHashIfNeeded(ctx, key, pkey, tair_hash_obj);
    return REDISMODULE_OK;
}

int tairHashTTLGenericFunc(RedisModuleCtx *ctx, RedisModuleString **argv, int argc, int unit) {
    RedisModule_AutoMemory(ctx);

    if (argc != 3) {
        return RedisModule_WrongArity(ctx);
    }

    int field_expired = 0;

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        RedisModule_ReplyWithLongLong(ctx, -2);
        return REDISMODULE_OK;
    } else {
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    RedisModuleString *pkey = argv[1], *skey = argv[2];

    int dbid = RedisModule_GetSelectedDb(ctx);
    if (fieldExpireIfNeeded(ctx, dbid, pkey, tair_hash_obj, skey, 0)) {
        field_expired = 1;
    }

    TairHashVal *tair_hash_val = (TairHashVal *)m_dictFetchValue(tair_hash_obj->hash, skey);
    if (field_expired || tair_hash_val == NULL) {
        RedisModule_ReplyWithLongLong(ctx, -3);
    } else {
        if (tair_hash_val->expire == 0) {
            RedisModule_ReplyWithLongLong(ctx, -1);
        } else {
            long long ttl = tair_hash_val->expire - RedisModule_Milliseconds();
            if (ttl < 0) {
                ttl = 0;
            }
            if (UNIT_SECONDS == unit) {
                RedisModule_ReplyWithLongLong(ctx, (ttl + 500) / 1000);
            } else {
                RedisModule_ReplyWithLongLong(ctx, ttl);
            }
        }
    }

    delEmptyTairHashIfNeeded(ctx, key, pkey, tair_hash_obj);
    return REDISMODULE_OK;
}

int mstring2ld(RedisModuleString *val, long double *r_val) {
    if (!val)
        return REDISMODULE_ERR;

    size_t t_len;
    const char *t_ptr = RedisModule_StringPtrLen(val, &t_len);
    if (m_string2ld(t_ptr, t_len, r_val) == 0) {
        return REDISMODULE_ERR;
    }

    return REDISMODULE_OK;
}

/* ========================= "tairhash" type commands ======================= */

/* EXHSET <key> <field> <value> [EX time] [EXAT time] [PX time] [PXAT time] [NX|XX] [VER version | ABS version] [KEEPTTL] */
int TairHashTypeHset_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc < 4) {
        return RedisModule_WrongArity(ctx);
    }

    /* Read before any key or field lookup: the clock read serializes the pipeline
     * (ISB on arm64, lfence/rdtscp on x86), stalling on any cache miss still in flight. */
    long long now = seqClockNow();

    long long milliseconds = 0, expire = 0, version = 0;
    RedisModuleString *expire_p = NULL, *version_p = NULL;
    int ex_flags = TAIR_HASH_SET_NO_FLAGS;
    int nokey = 0;

    g_expire_algorithm.passiveExpire(ctx, RedisModule_GetSelectedDb(ctx), argv[1]);

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    for (int j = 4; j < argc; j++) {
        RedisModuleString *next = (j == argc - 1) ? NULL : argv[j + 1];
        if (!mstrcasecmp(argv[j], "nx") && !(ex_flags & TAIR_HASH_SET_XX)) {
            ex_flags |= TAIR_HASH_SET_NX;
        } else if (!mstrcasecmp(argv[j], "xx") && !(ex_flags & TAIR_HASH_SET_NX)) {
            ex_flags |= TAIR_HASH_SET_XX;
        } else if (!mstrcasecmp(argv[j], "ex") && !(ex_flags & TAIR_HASH_SET_PX) && !(ex_flags & TAIR_HASH_SET_EX) && !(ex_flags & TAIR_HASH_SET_KEEPTTL) && next) {
            ex_flags |= TAIR_HASH_SET_EX;
            expire_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "exat") && !(ex_flags & TAIR_HASH_SET_PX) && !(ex_flags & TAIR_HASH_SET_EX) && !(ex_flags & TAIR_HASH_SET_KEEPTTL) && next) {
            ex_flags |= TAIR_HASH_SET_EX;
            ex_flags |= TAIR_HASH_SET_ABS_EXPIRE;
            expire_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "px") && !(ex_flags & TAIR_HASH_SET_PX) && !(ex_flags & TAIR_HASH_SET_EX) && !(ex_flags & TAIR_HASH_SET_KEEPTTL) && next) {
            ex_flags |= TAIR_HASH_SET_PX;
            expire_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "pxat") && !(ex_flags & TAIR_HASH_SET_PX) && !(ex_flags & TAIR_HASH_SET_EX) && !(ex_flags & TAIR_HASH_SET_KEEPTTL) && next) {
            ex_flags |= TAIR_HASH_SET_PX;
            ex_flags |= TAIR_HASH_SET_ABS_EXPIRE;
            expire_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "ver") && !(ex_flags & TAIR_HASH_SET_WITH_ABS_VER) && !(ex_flags & TAIR_HASH_SET_WITH_GT_VER) && next) {
            ex_flags |= TAIR_HASH_SET_WITH_VER;
            version_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "abs") && !(ex_flags & TAIR_HASH_SET_WITH_VER) && !(ex_flags & TAIR_HASH_SET_WITH_GT_VER) && next) {
            ex_flags |= TAIR_HASH_SET_WITH_ABS_VER;
            version_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "gt") && !(ex_flags & TAIR_HASH_SET_WITH_VER) && !(ex_flags & TAIR_HASH_SET_WITH_ABS_VER) && next) {
            ex_flags |= TAIR_HASH_SET_WITH_GT_VER;
            version_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "keepttl") && !(ex_flags & TAIR_HASH_SET_EX) && !(ex_flags & TAIR_HASH_SET_PX)) {
            ex_flags |= TAIR_HASH_SET_KEEPTTL;
        } else {
            RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
            return REDISMODULE_ERR;
        }
    }

    if ((NULL != expire_p) && (RedisModule_StringToLongLong(expire_p, &expire) != REDISMODULE_OK)) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    if (expire_p && expire < 0) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    if ((NULL != version_p) && (RedisModule_StringToLongLong(version_p, &version) != REDISMODULE_OK)) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    if (version < 0 || ((ex_flags & (TAIR_HASH_SET_WITH_ABS_VER | TAIR_HASH_SET_WITH_GT_VER)) && version == 0)) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    RedisModuleString *pkey = argv[1], *skey = argv[2];

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        if (ex_flags & TAIR_HASH_SET_XX) {
            RedisModule_ReplyWithLongLong(ctx, -1);
            return REDISMODULE_ERR;
        }
        tair_hash_obj = createTairHashTypeObject();
        tair_hash_obj->key = RedisModule_CreateStringFromString(NULL, pkey);
        RedisModule_ModuleTypeSetValue(key, TairHashType, tair_hash_obj);
    } else {
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    int dbid = RedisModule_GetSelectedDb(ctx);
    fieldExpireIfNeeded(ctx, dbid, pkey, tair_hash_obj, skey, 0);
    TairHashVal *tair_hash_val = (TairHashVal *)m_dictFetchValue(tair_hash_obj->hash, skey);
    if (tair_hash_val == NULL) {
        if (ex_flags & TAIR_HASH_SET_XX) {
            RedisModule_ReplyWithLongLong(ctx, -1);
            return REDISMODULE_ERR;
        }
        nokey = 1;
        tair_hash_val = createTairHashVal();
        tair_hash_val->version = 0;
        tair_hash_val->expire = 0;
        tair_hash_val->value = NULL;
    } else {
        nokey = 0;
        if (ex_flags & TAIR_HASH_SET_NX) {
            RedisModule_ReplyWithLongLong(ctx, -1);
            return REDISMODULE_ERR;
        }

        /* Version equals 0 means no version checking */
        if (ex_flags & TAIR_HASH_SET_WITH_VER) {
            if (version != 0 && version != tair_hash_val->version) {
                RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_VERSION);
                return REDISMODULE_ERR;
            }
        } else if (ex_flags & TAIR_HASH_SET_WITH_GT_VER) {
            if (version <= tair_hash_val->version) {
                RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_VERSION);
                return REDISMODULE_ERR;
            }
        }
    }

    if (ex_flags & (TAIR_HASH_SET_WITH_ABS_VER | TAIR_HASH_SET_WITH_GT_VER)) {
        tair_hash_val->version = version;
    } else {
        tair_hash_val->version += 1;
    }

    if (0 < expire) {
        if (ex_flags & TAIR_HASH_SET_EX) {
            expire *= 1000;
        }
        if (ex_flags & TAIR_HASH_SET_ABS_EXPIRE) {
            milliseconds = expire;
        } else {
            milliseconds = RedisModule_Milliseconds() + expire;
        }
    } else if (expire_p && expire == 0) {
        milliseconds = 1;
    }

    if (milliseconds == 0 && !(ex_flags & TAIR_HASH_SET_KEEPTTL)) {
        g_expire_algorithm.delete(ctx, dbid, argv[1], tair_hash_obj, skey, tair_hash_val->expire);
        tair_hash_val->expire = 0;
    }

    if (milliseconds > 0) {
        if (nokey || tair_hash_val->expire == 0) {
            g_expire_algorithm.insert(ctx, dbid, argv[1], tair_hash_obj, skey, milliseconds);
        } else {
            g_expire_algorithm.update(ctx, dbid, argv[1], tair_hash_obj, skey, tair_hash_val->expire, milliseconds);
        }
        tair_hash_val->expire = milliseconds;
    }

    if (tair_hash_val->value) {
        RedisModule_FreeString(NULL, tair_hash_val->value);
    }

    tair_hash_val->value = takeAndRef(argv[3]);
    tair_hash_val->seq = tairHashNextSeq(now);
    if (nokey) {
        m_dictAdd(tair_hash_obj->hash, takeAndRef(skey), tair_hash_val);
        RedisModule_ReplyWithLongLong(ctx, 1);
    } else {
        RedisModule_ReplyWithLongLong(ctx, 0);
    }

    replicateFieldState(ctx, argv[1], argv[2], tair_hash_val);
    return REDISMODULE_OK;
}

/* EXHSETNX <key> <field> <value> */
int TairHashTypeHsetNx_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc != 4) {
        return RedisModule_WrongArity(ctx);
    }

    /* Read before any key or field lookup: the clock read serializes the pipeline
     * (ISB on arm64, lfence/rdtscp on x86), stalling on any cache miss still in flight. */
    long long now = seqClockNow();

    g_expire_algorithm.passiveExpire(ctx, RedisModule_GetSelectedDb(ctx), argv[1]);

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    RedisModuleString *pkey = argv[1], *skey = argv[2], *svalue = argv[3];

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        tair_hash_obj = createTairHashTypeObject();
        tair_hash_obj->key = RedisModule_CreateStringFromString(NULL, pkey);
        RedisModule_ModuleTypeSetValue(key, TairHashType, tair_hash_obj);
    } else {
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    TairHashVal *tair_hash_val = (TairHashVal *)m_dictFetchValue(tair_hash_obj->hash, skey);
    if (tair_hash_val == NULL) {
        tair_hash_val = createTairHashVal();
        tair_hash_val->expire = 0;
    } else {
        RedisModule_ReplyWithLongLong(ctx, 0);
        return REDISMODULE_OK;
    }

    /* A new field starts at version 1, as with EXHSET; version 0 means "no version". */
    tair_hash_val->version = 1;
    tair_hash_val->value = takeAndRef(svalue);
    tair_hash_val->seq = tairHashNextSeq(now);
    m_dictAdd(tair_hash_obj->hash, takeAndRef(skey), tair_hash_val);

    replicateFieldState(ctx, pkey, skey, tair_hash_val);
    RedisModule_ReplyWithLongLong(ctx, 1);
    return REDISMODULE_OK;
}

/* EXHMSET key field value [field value …] */
int TairHashTypeHmset_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if ((argc % 2) == 1) {
        return RedisModule_WrongArity(ctx);
    }

    /* Read before any key or field lookup: the clock read serializes the pipeline
     * (ISB on arm64, lfence/rdtscp on x86), stalling on any cache miss still in flight. */
    long long now = seqClockNow();

    g_expire_algorithm.passiveExpire(ctx, RedisModule_GetSelectedDb(ctx), argv[1]);

    int nokey;
    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        tair_hash_obj = createTairHashTypeObject();
        tair_hash_obj->key = RedisModule_CreateStringFromString(NULL, argv[1]);
        RedisModule_ModuleTypeSetValue(key, TairHashType, tair_hash_obj);
    } else {
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    int dbid = RedisModule_GetSelectedDb(ctx);
    FieldStateBatch batch;
    fieldStateBatchInit(&batch, ctx, argv[1], (argc - 2) / 2);
    for (int i = 2; i < argc; i += 2) {
        fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, argv[i], 0);
        TairHashVal *tair_hash_val = (TairHashVal *)m_dictFetchValue(tair_hash_obj->hash, argv[i]);
        if (tair_hash_val == NULL) {
            nokey = 1;
            tair_hash_val = createTairHashVal();
            tair_hash_val->expire = 0;
        } else {
            nokey = 0;
            if (tair_hash_val->value) {
                RedisModule_FreeString(NULL, tair_hash_val->value);
            }
        }
        tair_hash_val->value = takeAndRef(argv[i + 1]);
        tair_hash_val->version++;
        tair_hash_val->seq = tairHashNextSeq(now);
        if (nokey) {
            m_dictAdd(tair_hash_obj->hash, takeAndRef(argv[i]), tair_hash_val);
        }
        fieldStateBatchAdd(&batch, argv[i], tair_hash_val, NULL);
    }
    fieldStateBatchFlush(&batch);

    RedisModule_ReplyWithSimpleString(ctx, "OK");
    return REDISMODULE_OK;
}

/* EXHMSETWITHOPTS tairHashkey field1 val1 ver1 expire1 [field2 val2 ver2 expire2 ...] */
int TairHashTypeHmsetWithOpts_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (((argc - 2) % 4) != 0) {
        return RedisModule_WrongArity(ctx);
    }

    /* Read before any key or field lookup: the clock read serializes the pipeline
     * (ISB on arm64, lfence/rdtscp on x86), stalling on any cache miss still in flight. */
    long long now = seqClockNow();

    g_expire_algorithm.passiveExpire(ctx, RedisModule_GetSelectedDb(ctx), argv[1]);

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        tair_hash_obj = createTairHashTypeObject();
        tair_hash_obj->key = RedisModule_CreateStringFromString(NULL, argv[1]);
        RedisModule_ModuleTypeSetValue(key, TairHashType, tair_hash_obj);
    } else {
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    long long ver;
    long long when;

    int nokey;
    int dbid = RedisModule_GetSelectedDb(ctx);
    for (int i = 2; i < argc; i += 4) {
        if (RedisModule_StringToLongLong(argv[i + 3], &when) != REDISMODULE_OK) {
            RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
            return REDISMODULE_ERR;
        }

        if (RedisModule_StringToLongLong(argv[i + 2], &ver) != REDISMODULE_OK) {
            RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
            return REDISMODULE_ERR;
        }

        if (ver < 0 || when < 0) {
            RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
            return REDISMODULE_ERR;
        }

        fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, argv[i], 0);
        TairHashVal *tair_hash_val = (TairHashVal *)m_dictFetchValue(tair_hash_obj->hash, argv[i]);
        if (tair_hash_val == NULL || ver == 0 || tair_hash_val->version == ver) {
            continue;
        } else {
            RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_VERSION);
            return REDISMODULE_ERR;
        }
    }

    FieldStateBatch batch;
    fieldStateBatchInit(&batch, ctx, argv[1], (argc - 2) / 4);
    for (int i = 2; i < argc; i += 4) {
        /* Validated by the loop above. */
        RedisModule_StringToLongLong(argv[i + 3], &when);

        TairHashVal *tair_hash_val = (TairHashVal *)m_dictFetchValue(tair_hash_obj->hash, argv[i]);
        if (tair_hash_val == NULL) {
            tair_hash_val = createTairHashVal();
            tair_hash_val->expire = 0;
            tair_hash_val->version = 0;
            tair_hash_val->value = NULL;
            nokey = 1;
        } else {
            nokey = 0;
        }

        if (tair_hash_val->value) {
            RedisModule_FreeString(NULL, tair_hash_val->value);
        }

        tair_hash_val->value = takeAndRef(argv[i + 1]);
        tair_hash_val->version++;
        tair_hash_val->seq = tairHashNextSeq(now);

        int dbid = RedisModule_GetSelectedDb(ctx);
        when = RedisModule_Milliseconds() + when * 1000;
        if (nokey || tair_hash_val->expire == 0) {
            g_expire_algorithm.insert(ctx, dbid, argv[1], tair_hash_obj, argv[i], when);
        } else {
            g_expire_algorithm.update(ctx, dbid, argv[1], tair_hash_obj, argv[i], tair_hash_val->expire, when);
        }
        tair_hash_val->expire = when;

        if (nokey) {
            m_dictAdd(tair_hash_obj->hash, takeAndRef(argv[i]), tair_hash_val);
        }

        fieldStateBatchAdd(&batch, argv[i], tair_hash_val, NULL);
    }
    fieldStateBatchFlush(&batch);

    RedisModule_ReplyWithSimpleString(ctx, "OK");
    return REDISMODULE_OK;
}

/*  EXHPEXPIREAT <key> <field> <milliseconds-timestamp> [ VER version | ABS version | GT version ]*/
int TairHashTypeHpexpireAt_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    return tairHashExpireGenericFunc(ctx, argv, argc, 0, UNIT_MILLISECONDS);
}

/*  EXHPEXPIRE <key> <field> <milliseconds> [ VER version | ABS version | GT version ]*/
int TairHashTypeHpexpire_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    return tairHashExpireGenericFunc(ctx, argv, argc, RedisModule_Milliseconds(), UNIT_MILLISECONDS);
}

/*  EXHEXPIREAT <key> <field> <timestamp> [ VER version | ABS version | GT version ] */
int TairHashTypeHexpireAt_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    return tairHashExpireGenericFunc(ctx, argv, argc, 0, UNIT_SECONDS);
}

/*  EXHEXPIRE <key> <field> <seconds> [ VER version | ABS version | GT version ] */
int TairHashTypeHexpire_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    return tairHashExpireGenericFunc(ctx, argv, argc, RedisModule_Milliseconds(), UNIT_SECONDS);
}

/*  EXHPTTL <key> <field> */
int TairHashTypeHpttl_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    return tairHashTTLGenericFunc(ctx, argv, argc, UNIT_MILLISECONDS);
}

/*  EXHTTL <key> <field> */
int TairHashTypeHttl_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    return tairHashTTLGenericFunc(ctx, argv, argc, UNIT_SECONDS);
}

/* EXHPERSIST <key> <field> */
int TairHashTypeHpersist_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc != 3) {
        return RedisModule_WrongArity(ctx);
    }

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        RedisModule_ReplyWithLongLong(ctx, 0);
        return REDISMODULE_OK;
    } else {
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    int dbid = RedisModule_GetSelectedDb(ctx);
    if (fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, argv[2], 0)) {
        RedisModule_ReplyWithLongLong(ctx, 0);
        return REDISMODULE_OK;
    }

    TairHashVal *tair_hash_val = (TairHashVal *)m_dictFetchValue(tair_hash_obj->hash, argv[2]);
    if (tair_hash_val == NULL) {
        RedisModule_ReplyWithLongLong(ctx, 0);
        return REDISMODULE_OK;
    }

    if (!tair_hash_val->expire) {
        RedisModule_ReplyWithLongLong(ctx, 0);
    } else {
        int dbid = RedisModule_GetSelectedDb(ctx);
        g_expire_algorithm.delete(ctx, dbid, argv[1], tair_hash_obj, argv[2], tair_hash_val->expire);
        tair_hash_val->expire = 0;
        replicateFieldState(ctx, argv[1], argv[2], tair_hash_val);
        RedisModule_ReplyWithLongLong(ctx, 1);
    }

    return REDISMODULE_OK;
}

/*  EXHVER <key> <field> */
int TairHashTypeHver_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc != 3) {
        return RedisModule_WrongArity(ctx);
    }

    int field_expired = 0;

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        RedisModule_ReplyWithLongLong(ctx, -1);
        return REDISMODULE_OK;
    } else {
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    int dbid = RedisModule_GetSelectedDb(ctx);
    if (fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, argv[2], 0)) {
        field_expired = 1;
    }

    TairHashVal *tair_hash_val = (TairHashVal *)m_dictFetchValue(tair_hash_obj->hash, argv[2]);
    if (field_expired || tair_hash_val == NULL) {
        RedisModule_ReplyWithLongLong(ctx, -2);
    } else {
        RedisModule_ReplyWithLongLong(ctx, tair_hash_val->version);
    }

    delEmptyTairHashIfNeeded(ctx, key, argv[1], tair_hash_obj);
    return REDISMODULE_OK;
}

/*  EXHSETVER <key> <field> <version> */
int TairHashTypeHsetVer_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc != 4) {
        return RedisModule_WrongArity(ctx);
    }

    long long version;

    if (RedisModule_StringToLongLong(argv[3], &version) != REDISMODULE_OK) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    /* version must > 0 */
    if (version <= 0) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    g_expire_algorithm.passiveExpire(ctx, RedisModule_GetSelectedDb(ctx), argv[1]);

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        RedisModule_ReplyWithLongLong(ctx, 0);
        return REDISMODULE_OK;
    } else {
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    TairHashVal *tair_hash_val = (TairHashVal *)m_dictFetchValue(tair_hash_obj->hash, argv[2]);
    if (tair_hash_val == NULL) {
        RedisModule_ReplyWithLongLong(ctx, 0);
        return REDISMODULE_OK;
    }

    int dbid = RedisModule_GetSelectedDb(ctx);
    if (fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, argv[2], 0)) {
        delEmptyTairHashIfNeeded(ctx, key, argv[1], tair_hash_obj);
        RedisModule_ReplyWithLongLong(ctx, 0);
        return REDISMODULE_OK;
    }

    tair_hash_val->version = version;
    RedisModule_ReplyWithLongLong(ctx, 1);
    replicateFieldState(ctx, argv[1], argv[2], tair_hash_val);
    return REDISMODULE_OK;
}

/* EXHINCRBY <key> <field> <value> [EX time] [EXAT time] [PX time] [PXAT time] [VER version | ABS version | GT version] [MIN minval] [MAX maxval] [KEEPTTL] */
int TairHashTypeHincrBy_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc < 4) {
        return RedisModule_WrongArity(ctx);
    }

    /* Read before any key or field lookup: the clock read serializes the pipeline
     * (ISB on arm64, lfence/rdtscp on x86), stalling on any cache miss still in flight. */
    long long now = seqClockNow();

    long long milliseconds = 0, expire = 0, incr = 0, version = 0, min = 0, max = 0;
    RedisModuleString *expire_p = NULL;
    RedisModuleString *version_p = NULL;
    RedisModuleString *min_p = NULL, *max_p = NULL;
    int ex_flags = TAIR_HASH_SET_NO_FLAGS;
    int nokey;

    g_expire_algorithm.passiveExpire(ctx, RedisModule_GetSelectedDb(ctx), argv[1]);

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    if (RedisModule_StringToLongLong(argv[3], &incr) != REDISMODULE_OK) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_NOT_INTEGER);
        return REDISMODULE_ERR;
    }

    for (int j = 4; j < argc; j++) {
        RedisModuleString *next = (j == argc - 1) ? NULL : argv[j + 1];
        if (!mstrcasecmp(argv[j], "ex") && !(ex_flags & TAIR_HASH_SET_PX) && !(ex_flags & TAIR_HASH_SET_EX) && !(ex_flags & TAIR_HASH_SET_KEEPTTL) && next) {
            ex_flags |= TAIR_HASH_SET_EX;
            expire_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "exat") && !(ex_flags & TAIR_HASH_SET_PX) && !(ex_flags & TAIR_HASH_SET_EX) && !(ex_flags & TAIR_HASH_SET_KEEPTTL) && next) {
            ex_flags |= TAIR_HASH_SET_EX;
            ex_flags |= TAIR_HASH_SET_ABS_EXPIRE;
            expire_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "px") && !(ex_flags & TAIR_HASH_SET_PX) && !(ex_flags & TAIR_HASH_SET_EX) && !(ex_flags & TAIR_HASH_SET_KEEPTTL) && next) {
            ex_flags |= TAIR_HASH_SET_PX;
            expire_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "pxat") && !(ex_flags & TAIR_HASH_SET_PX) && !(ex_flags & TAIR_HASH_SET_EX) && !(ex_flags & TAIR_HASH_SET_KEEPTTL) && next) {
            ex_flags |= TAIR_HASH_SET_PX;
            ex_flags |= TAIR_HASH_SET_ABS_EXPIRE;
            expire_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "ver") && !(ex_flags & TAIR_HASH_SET_WITH_ABS_VER) && !(ex_flags & TAIR_HASH_SET_WITH_GT_VER) && next) {
            ex_flags |= TAIR_HASH_SET_WITH_VER;
            version_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "abs") && !(ex_flags & TAIR_HASH_SET_WITH_VER) && !(ex_flags & TAIR_HASH_SET_WITH_GT_VER) && next) {
            ex_flags |= TAIR_HASH_SET_WITH_ABS_VER;
            version_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "gt") && !(ex_flags & TAIR_HASH_SET_WITH_VER) && !(ex_flags & TAIR_HASH_SET_WITH_ABS_VER) && next) {
            ex_flags |= TAIR_HASH_SET_WITH_GT_VER;
            version_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "min") && next) {
            ex_flags |= TAIR_HASH_SET_WITH_BOUNDARY;
            min_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "max") && next) {
            ex_flags |= TAIR_HASH_SET_WITH_BOUNDARY;
            max_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "keepttl") && !(ex_flags & TAIR_HASH_SET_EX) && !(ex_flags & TAIR_HASH_SET_PX)) {
            ex_flags |= TAIR_HASH_SET_KEEPTTL;
        } else {
            RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
            return REDISMODULE_ERR;
        }
    }

    if ((NULL != expire_p) && (RedisModule_StringToLongLong(expire_p, &expire) != REDISMODULE_OK)) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    if (expire_p && expire < 0) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    if ((NULL != version_p) && (RedisModule_StringToLongLong(version_p, &version) != REDISMODULE_OK)) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    if (version < 0 || ((ex_flags & (TAIR_HASH_SET_WITH_ABS_VER | TAIR_HASH_SET_WITH_GT_VER)) && version == 0)) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    if ((NULL != min_p) && (RedisModule_StringToLongLong(min_p, &min))) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INT_MIN_MAX);
        return REDISMODULE_ERR;
    }

    if ((NULL != max_p) && (RedisModule_StringToLongLong(max_p, &max))) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INT_MIN_MAX);
        return REDISMODULE_ERR;
    }

    if (NULL != min_p && NULL != max_p && max < min) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_MIN_MAX);
        return REDISMODULE_ERR;
    }

    RedisModuleString *pkey = argv[1], *skey = argv[2];

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        tair_hash_obj = createTairHashTypeObject();
        tair_hash_obj->key = RedisModule_CreateStringFromString(NULL, pkey);
        RedisModule_ModuleTypeSetValue(key, TairHashType, tair_hash_obj);
    } else {
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    int dbid = RedisModule_GetSelectedDb(ctx);
    fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, argv[2], 0);
    TairHashVal *tair_hash_val = NULL;
    m_dictEntry *de = m_dictFind(tair_hash_obj->hash, skey);
    if (de == NULL) {
        nokey = 1;
        tair_hash_val = createTairHashVal();
        tair_hash_val->expire = 0;
        tair_hash_val->version = 0;
    } else {
        nokey = 0;
        tair_hash_val = dictGetVal(de);
        skey = dictGetKey(de);
    }

    long long cur_val;
    if (type == REDISMODULE_KEYTYPE_EMPTY || nokey) {
        tair_hash_val->value = RedisModule_CreateStringFromLongLong(NULL, 0);
        cur_val = 0;
        tair_hash_val->version = 0;
    } else {
        if (RedisModule_StringToLongLong(tair_hash_val->value, &cur_val) != REDISMODULE_OK) {
            RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_NOT_INTEGER);
            return REDISMODULE_ERR;
        }

        /* Version equals 0 means no version checking */
        if (ex_flags & TAIR_HASH_SET_WITH_VER) {
            if (version != 0 && version != tair_hash_val->version) {
                RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_VERSION);
                return REDISMODULE_ERR;
            }
        } else if (ex_flags & TAIR_HASH_SET_WITH_GT_VER) {
            if (version <= tair_hash_val->version) {
                RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_VERSION);
                return REDISMODULE_ERR;
            }
        }
    }

    if ((incr < 0 && cur_val < 0 && incr < (LLONG_MIN - cur_val)) || (incr > 0 && cur_val > 0 && incr > (LLONG_MAX - cur_val)) || (max_p != NULL && cur_val + incr > max) || (min_p != NULL && cur_val + incr < min)) {
        if (nokey) {
            tairHashValRelease(tair_hash_val);
        }
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_OVERFLOW);
        return REDISMODULE_ERR;
    }

    if (ex_flags & (TAIR_HASH_SET_WITH_ABS_VER | TAIR_HASH_SET_WITH_GT_VER)) {
        tair_hash_val->version = version;
    } else {
        tair_hash_val->version += 1;
    }

    cur_val += incr;

    if (tair_hash_val->value) {
        RedisModule_FreeString(NULL, tair_hash_val->value);
    }
    tair_hash_val->value = RedisModule_CreateStringFromLongLong(NULL, cur_val);

    if (0 < expire) {
        if (ex_flags & TAIR_HASH_SET_EX) {
            expire *= 1000;
        }
        if (ex_flags & TAIR_HASH_SET_ABS_EXPIRE) {
            milliseconds = expire;
        } else {
            milliseconds = RedisModule_Milliseconds() + expire;
        }
    } else if (expire_p && expire == 0) {
        milliseconds = 1;
    }

    if (milliseconds == 0 && !(ex_flags & TAIR_HASH_SET_KEEPTTL)) {
        g_expire_algorithm.delete(ctx, dbid, argv[1], tair_hash_obj, skey, tair_hash_val->expire);
        tair_hash_val->expire = 0;
    }

    if (milliseconds > 0) {
        if (nokey || tair_hash_val->expire == 0) {
            g_expire_algorithm.insert(ctx, dbid, argv[1], tair_hash_obj, skey, milliseconds);
        } else {
            g_expire_algorithm.update(ctx, dbid, argv[1], tair_hash_obj, skey, tair_hash_val->expire, milliseconds);
        }
        tair_hash_val->expire = milliseconds;
    }

    tair_hash_val->seq = tairHashNextSeq(now);
    if (nokey) {
        m_dictAdd(tair_hash_obj->hash, takeAndRef(skey), tair_hash_val);
    }

    replicateFieldState(ctx, argv[1], argv[2], tair_hash_val);
    RedisModule_ReplyWithLongLong(ctx, cur_val);
    return REDISMODULE_OK;
}

/* EXHINCRBYFLOAT <key> <field> <value> [EX time] [EXAT time] [PX time] [PXAT time] [VER version | ABS version | GT version] [MIN
 * minval] [MAX maxval] [KEEPTTL] */
int TairHashTypeHincrByFloat_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc < 4) {
        return RedisModule_WrongArity(ctx);
    }

    /* Read before any key or field lookup: the clock read serializes the pipeline
     * (ISB on arm64, lfence/rdtscp on x86), stalling on any cache miss still in flight. */
    long long now = seqClockNow();

    long long milliseconds = 0, expire = 0, version = 0;
    long double incr = 0, min = 0, max = 0;
    RedisModuleString *expire_p = NULL;
    RedisModuleString *version_p = NULL;
    RedisModuleString *min_p = NULL, *max_p = NULL;
    int ex_flags = TAIR_HASH_SET_NO_FLAGS;
    int nokey = 0;

    g_expire_algorithm.passiveExpire(ctx, RedisModule_GetSelectedDb(ctx), argv[1]);

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    if (mstring2ld(argv[3], &incr) == REDISMODULE_ERR) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_NOT_FLOAT);
        return REDISMODULE_ERR;
    }

    for (int j = 4; j < argc; j++) {
        RedisModuleString *next = (j == argc - 1) ? NULL : argv[j + 1];

        if (!mstrcasecmp(argv[j], "ex") && !(ex_flags & TAIR_HASH_SET_PX) && !(ex_flags & TAIR_HASH_SET_EX) && !(ex_flags & TAIR_HASH_SET_KEEPTTL) && next) {
            ex_flags |= TAIR_HASH_SET_EX;
            expire_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "exat") && !(ex_flags & TAIR_HASH_SET_PX) && !(ex_flags & TAIR_HASH_SET_EX) && !(ex_flags & TAIR_HASH_SET_KEEPTTL) && next) {
            ex_flags |= TAIR_HASH_SET_EX;
            ex_flags |= TAIR_HASH_SET_ABS_EXPIRE;
            expire_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "px") && !(ex_flags & TAIR_HASH_SET_PX) && !(ex_flags & TAIR_HASH_SET_EX) && !(ex_flags & TAIR_HASH_SET_KEEPTTL) && next) {
            ex_flags |= TAIR_HASH_SET_PX;
            expire_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "pxat") && !(ex_flags & TAIR_HASH_SET_PX) && !(ex_flags & TAIR_HASH_SET_EX) && !(ex_flags & TAIR_HASH_SET_KEEPTTL) && next) {
            ex_flags |= TAIR_HASH_SET_PX;
            ex_flags |= TAIR_HASH_SET_ABS_EXPIRE;
            expire_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "ver") && !(ex_flags & TAIR_HASH_SET_WITH_ABS_VER) && !(ex_flags & TAIR_HASH_SET_WITH_GT_VER) && next) {
            ex_flags |= TAIR_HASH_SET_WITH_VER;
            version_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "abs") && !(ex_flags & TAIR_HASH_SET_WITH_VER) && !(ex_flags & TAIR_HASH_SET_WITH_GT_VER) && next) {
            ex_flags |= TAIR_HASH_SET_WITH_ABS_VER;
            version_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "gt") && !(ex_flags & TAIR_HASH_SET_WITH_VER) && !(ex_flags & TAIR_HASH_SET_WITH_ABS_VER) && next) {
            ex_flags |= TAIR_HASH_SET_WITH_GT_VER;
            version_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "min") && next) {
            ex_flags |= TAIR_HASH_SET_WITH_BOUNDARY;
            min_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "max") && next) {
            ex_flags |= TAIR_HASH_SET_WITH_BOUNDARY;
            max_p = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "keepttl") && !(ex_flags & TAIR_HASH_SET_EX) && !(ex_flags & TAIR_HASH_SET_PX)) {
            ex_flags |= TAIR_HASH_SET_KEEPTTL;
        } else {
            RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
            return REDISMODULE_ERR;
        }
    }

    if ((NULL != expire_p) && (RedisModule_StringToLongLong(expire_p, &expire) != REDISMODULE_OK)) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    if (expire_p && expire < 0) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    if ((NULL != version_p) && (RedisModule_StringToLongLong(version_p, &version) != REDISMODULE_OK)) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    if (version < 0 || ((ex_flags & (TAIR_HASH_SET_WITH_ABS_VER | TAIR_HASH_SET_WITH_GT_VER)) && version == 0)) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    if ((NULL != min_p) && (mstring2ld(min_p, &min) != REDISMODULE_OK)) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_FLOAT_MIN_MAX);
        return REDISMODULE_ERR;
    }

    if ((NULL != max_p) && (mstring2ld(max_p, &max) != REDISMODULE_OK)) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_FLOAT_MIN_MAX);
        return REDISMODULE_ERR;
    }

    if (NULL != min_p && NULL != max_p && max < min) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_MIN_MAX);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        tair_hash_obj = createTairHashTypeObject();
        tair_hash_obj->key = RedisModule_CreateStringFromString(NULL, argv[1]);
        RedisModule_ModuleTypeSetValue(key, TairHashType, tair_hash_obj);
    } else {
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    RedisModuleString *skey = argv[2];
    int dbid = RedisModule_GetSelectedDb(ctx);
    fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, argv[2], 0);
    m_dictEntry *de = m_dictFind(tair_hash_obj->hash, skey);
    TairHashVal *tair_hash_val = NULL;
    if (de == NULL) {
        nokey = 1;
        tair_hash_val = createTairHashVal();
        tair_hash_val->expire = 0;
        tair_hash_val->version = 0;
    } else {
        nokey = 0;
        tair_hash_val = dictGetVal(de);
        skey = dictGetKey(de);
    }

    long double cur_val;
    if (type == REDISMODULE_KEYTYPE_EMPTY || nokey) {
        tair_hash_val->value = RedisModule_CreateStringFromLongLong(NULL, 0);
        cur_val = 0;
        tair_hash_val->version = 0;
    } else {
        if (mstring2ld(tair_hash_val->value, &cur_val) != REDISMODULE_OK) {
            RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_NOT_FLOAT);
            return REDISMODULE_ERR;
        }

        /* Version equals 0 means no version checking */
        if (ex_flags & TAIR_HASH_SET_WITH_VER) {
            if (version != 0 && version != tair_hash_val->version) {
                RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_VERSION);
                return REDISMODULE_ERR;
            }
        } else if (ex_flags & TAIR_HASH_SET_WITH_GT_VER) {
            if (version <= tair_hash_val->version) {
                RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_VERSION);
                return REDISMODULE_ERR;
            }
        }
    }

    if (isnan(cur_val + incr) || isinf(cur_val + incr)) {
        if (nokey) {
            tairHashValRelease(tair_hash_val);
        }
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_OVERFLOW);
        return REDISMODULE_ERR;
    }

    if ((max_p != NULL && cur_val + incr > max) || (min_p != NULL && cur_val + incr < min)) {
        if (nokey) {
            tairHashValRelease(tair_hash_val);
        }
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_OVERFLOW);
        return REDISMODULE_ERR;
    }

    if (ex_flags & (TAIR_HASH_SET_WITH_ABS_VER | TAIR_HASH_SET_WITH_GT_VER)) {
        tair_hash_val->version = version;
    } else {
        tair_hash_val->version += 1;
    }

    cur_val += incr;

    char dbuf[MAX_LONG_DOUBLE_CHARS] = {0};
    int dlen = m_ld2string(dbuf, sizeof(dbuf), cur_val, 1);

    if (tair_hash_val->value) {
        RedisModule_FreeString(NULL, tair_hash_val->value);
    }
    tair_hash_val->value = RedisModule_CreateString(NULL, dbuf, dlen);

    if (0 < expire) {
        if (ex_flags & TAIR_HASH_SET_EX) {
            expire *= 1000;
        }
        if (ex_flags & TAIR_HASH_SET_ABS_EXPIRE) {
            milliseconds = expire;
        } else {
            milliseconds = RedisModule_Milliseconds() + expire;
        }
    } else if (expire_p && expire == 0) {
        milliseconds = 1;
    }

    if (milliseconds == 0 && !(ex_flags & TAIR_HASH_SET_KEEPTTL)) {
        g_expire_algorithm.delete(ctx, dbid, argv[1], tair_hash_obj, skey, tair_hash_val->expire);
        tair_hash_val->expire = 0;
    }

    if (milliseconds > 0) {
        if (nokey || tair_hash_val->expire == 0) {
            g_expire_algorithm.insert(ctx, dbid, argv[1], tair_hash_obj, skey, milliseconds);
        } else {
            g_expire_algorithm.update(ctx, dbid, argv[1], tair_hash_obj, skey, tair_hash_val->expire, milliseconds);
        }
        tair_hash_val->expire = milliseconds;
    }

    tair_hash_val->seq = tairHashNextSeq(now);
    if (nokey) {
        m_dictAdd(tair_hash_obj->hash, takeAndRef(skey), tair_hash_val);
    }

    replicateFieldState(ctx, argv[1], argv[2], tair_hash_val);
    RedisModule_ReplyWithString(ctx, tair_hash_val->value);
    return REDISMODULE_OK;
}

/* EXHGET <key> <field> */
int TairHashTypeHget_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc != 3) {
        return RedisModule_WrongArity(ctx);
    }

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        return RedisModule_ReplyWithNull(ctx);
    } else {
        if (RedisModule_ModuleTypeGetType(key) != TairHashType) {
            return RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        }
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    RedisModuleString *pkey = argv[1], *skey = argv[2];

    int field_expire = 0;
    int dbid = RedisModule_GetSelectedDb(ctx);
    if (fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, argv[2], 0)) {
        field_expire = 1;
    }

    TairHashVal *tair_hash_val = (TairHashVal *)m_dictFetchValue(tair_hash_obj->hash, skey);
    if (field_expire || tair_hash_val == NULL) {
        RedisModule_ReplyWithNull(ctx);
    } else {
        RedisModule_ReplyWithString(ctx, tair_hash_val->value);
    }

    delEmptyTairHashIfNeeded(ctx, key, pkey, tair_hash_obj);
    return REDISMODULE_OK;
}

/* EXHGETWITHVER <key> <field> */
int TairHashTypeHgetWithVer_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    if (argc != 3) {
        return RedisModule_WrongArity(ctx);
    }
    RedisModule_AutoMemory(ctx);

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        return RedisModule_ReplyWithNull(ctx);
    } else {
        if (RedisModule_ModuleTypeGetType(key) != TairHashType) {
            return RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        }
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    int field_expired = 0;
    int dbid = RedisModule_GetSelectedDb(ctx);
    if (fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, argv[2], 0)) {
        field_expired = 1;
    }

    TairHashVal *tair_hash_val = (TairHashVal *)m_dictFetchValue(tair_hash_obj->hash, argv[2]);
    if (field_expired || tair_hash_val == NULL) {
        return RedisModule_ReplyWithNull(ctx);
    } else {
        RedisModule_ReplyWithArray(ctx, 2);
        RedisModule_ReplyWithString(ctx, tair_hash_val->value);
        RedisModule_ReplyWithLongLong(ctx, tair_hash_val->version);
    }
    delEmptyTairHashIfNeeded(ctx, key, argv[1], tair_hash_obj);
    return REDISMODULE_OK;
}

/* EXHMGET key field [field ...] */
int TairHashTypeHmget_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc < 3) {
        return RedisModule_WrongArity(ctx);
    }

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        RedisModule_ReplyWithArray(ctx, argc - 2);
        for (int ii = 2; ii < argc; ++ii) {
            RedisModule_ReplyWithNull(ctx);
        }
        return REDISMODULE_OK;
    } else {
        if (RedisModule_ModuleTypeGetType(key) != TairHashType) {
            return RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        }
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    int dbid = RedisModule_GetSelectedDb(ctx);
    int cn = 0;
    RedisModule_ReplyWithArray(ctx, REDISMODULE_POSTPONED_ARRAY_LEN);
    for (int ii = 2; ii < argc; ++ii) {
        if (fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, argv[ii], 0)) {
            RedisModule_ReplyWithNull(ctx);
            ++cn;
            continue;
        }
        TairHashVal *tair_hash_val = (TairHashVal *)m_dictFetchValue(tair_hash_obj->hash, argv[ii]);
        if (tair_hash_val == NULL) {
            RedisModule_ReplyWithNull(ctx);
            ++cn;
        } else {
            RedisModule_ReplyWithString(ctx, tair_hash_val->value);
            ++cn;
        }
    }
    RedisModule_ReplySetArrayLength(ctx, cn);
    delEmptyTairHashIfNeeded(ctx, key, argv[1], tair_hash_obj);
    return REDISMODULE_OK;
}

/* EXHMGETWITHVER key field [field ...] */
int TairHashTypeHmgetWithVer_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    if (argc < 3) {
        return RedisModule_WrongArity(ctx);
    }
    RedisModule_AutoMemory(ctx);

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        RedisModule_ReplyWithArray(ctx, argc - 2);
        for (int ii = 2; ii < argc; ++ii) {
            RedisModule_ReplyWithNull(ctx);
        }
        return REDISMODULE_OK;
    } else {
        if (RedisModule_ModuleTypeGetType(key) != TairHashType) {
            return RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        }
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    int dbid = RedisModule_GetSelectedDb(ctx);
    int cn = 0;
    RedisModule_ReplyWithArray(ctx, REDISMODULE_POSTPONED_ARRAY_LEN);
    for (int ii = 2; ii < argc; ++ii) {
        if (fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, argv[ii], 0)) {
            RedisModule_ReplyWithNull(ctx);
            ++cn;
            continue;
        }
        TairHashVal *tair_hash_val = (TairHashVal *)m_dictFetchValue(tair_hash_obj->hash, argv[ii]);
        if (tair_hash_val == NULL) {
            RedisModule_ReplyWithNull(ctx);
            ++cn;
        } else {
            RedisModule_ReplyWithArray(ctx, 2);
            RedisModule_ReplyWithString(ctx, tair_hash_val->value);
            RedisModule_ReplyWithLongLong(ctx, tair_hash_val->version);
            ++cn;
        }
    }
    RedisModule_ReplySetArrayLength(ctx, cn);
    delEmptyTairHashIfNeeded(ctx, key, argv[1], tair_hash_obj);

    return REDISMODULE_OK;
}

/* EXHDEL <key> <field> <field> <field> ...*/
int TairHashTypeHdel_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc < 3) {
        return RedisModule_WrongArity(ctx);
    }

    long long j, deleted = 0;

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        return RedisModule_ReplyWithLongLong(ctx, 0);
    } else {
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    int dbid = RedisModule_GetSelectedDb(ctx);
    TairHashVal *tair_hash_val = NULL;
    for (j = 2; j < argc; j++) {
        /* Internal will perform RedisModule_Replicate EXHDEL for replication */
        fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, argv[j], 0);
        m_dictEntry *de = m_dictFind(tair_hash_obj->hash, argv[j]);
        if (de) {
            tair_hash_val = dictGetVal(de);
            if (tair_hash_val->expire > 0) {
                g_expire_algorithm.delete(ctx, dbid, argv[1], tair_hash_obj, argv[j], tair_hash_val->expire);
            }
            m_dictDelete(tair_hash_obj->hash, argv[j]);

            RedisModule_Replicate(ctx, "EXHDEL", "ss", argv[1], argv[j]);
            deleted++;
        }
    }

    delEmptyTairHashIfNeeded(ctx, key, argv[1], tair_hash_obj);
    RedisModule_ReplyWithLongLong(ctx, deleted);
    return REDISMODULE_OK;
}

/* Since using `RedisModule_Replicate` directly in the timer callback will generate nested MULTIs, we have
 * to generate a new internal command and then use `RedisModule_Call` to call it in the module. It is best
 * not to use this command directly in the client. */

/* EXHDELREPL <key> <field> */
int TairHashTypeHdelRepl_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc != 3) {
        return RedisModule_WrongArity(ctx);
    }

    long long deleted = 0;

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        return RedisModule_ReplyWithLongLong(ctx, 0);
    } else {
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    int dbid = RedisModule_GetSelectedDb(ctx);
    TairHashVal *tair_hash_val = NULL;
    m_dictEntry *de = m_dictFind(tair_hash_obj->hash, argv[2]);
    if (de) {
        m_dictDelete(tair_hash_obj->hash, argv[2]);
        RedisModule_Replicate(ctx, "EXHDEL", "ss", argv[1], argv[2]);
        deleted++;
    }

    RedisModule_ReplyWithLongLong(ctx, deleted);
    return REDISMODULE_OK;
}

/* EXHDELWITHVER <key> <field> version> <field> <version> ...*/
int TairHashTypeHdelWithVer_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc < 4 || ((argc - 2) % 2) != 0) {
        return RedisModule_WrongArity(ctx);
    }

    long long j, deleted = 0;

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        return RedisModule_ReplyWithLongLong(ctx, 0);
    } else {
        if (RedisModule_ModuleTypeGetType(key) != TairHashType) {
            return RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        }
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    long long ver;

    long long before_min_score, after_min_score;
    int dbid = RedisModule_GetSelectedDb(ctx);
    for (j = 2; j < argc; j += 2) {
        if (RedisModule_StringToLongLong(argv[j + 1], &ver) != REDISMODULE_OK) {
            RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
            return REDISMODULE_ERR;
        }

        /* Internal will perform RedisModule_Replicate EXHDEL for replication */
        fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, argv[j], 0);

        TairHashVal *tair_hash_val = (TairHashVal *)m_dictFetchValue(tair_hash_obj->hash, argv[j]);
        if (tair_hash_val != NULL) {
            if (ver == 0 || ver == tair_hash_val->version) {
                if (tair_hash_val->expire > 0) {
                    g_expire_algorithm.delete(ctx, dbid, argv[1], tair_hash_obj, argv[j], tair_hash_val->expire);
                }
                m_dictDelete(tair_hash_obj->hash, argv[j]);
                RedisModule_Replicate(ctx, "EXHDEL", "ss", argv[1], argv[j]);
                deleted++;
            }
        }
    }

    delEmptyTairHashIfNeeded(ctx, key, argv[1], tair_hash_obj);
    RedisModule_ReplyWithLongLong(ctx, deleted);
    return REDISMODULE_OK;
}

/* EXHLEN <key> [noexp]*/
int TairHashTypeHlen_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);
    int noexp = 0;
    uint64_t len = 0;

    if (argc != 2 && argc != 3) {
        return RedisModule_WrongArity(ctx);
    }

    if (argc == 3) {
        if (!mstrcasecmp(argv[2], "noexp")) {
            noexp = 1;
        } else {
            RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
            return REDISMODULE_ERR;
        }
    }

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        return RedisModule_ReplyWithLongLong(ctx, 0);
    } else {
        if (RedisModule_ModuleTypeGetType(key) != TairHashType) {
            return RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        }
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    m_dictIterator *di;
    m_dictEntry *de;

    if (noexp) {
        TairHashVal *data;
        di = m_dictGetIterator(tair_hash_obj->hash);
        while ((de = m_dictNext(di)) != NULL) {
            data = (TairHashVal *)dictGetVal(de);
            if (isExpire(data->expire)) {
                continue;
            }
            len++;
        }
        m_dictReleaseIterator(di);
    } else {
        len = dictSize(tair_hash_obj->hash);
    }

    RedisModule_ReplyWithLongLong(ctx, len);
    return REDISMODULE_OK;
}

/* EXHEXISTS key field */
int TairHashTypeHexists_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc != 3) {
        return RedisModule_WrongArity(ctx);
    }

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        return RedisModule_ReplyWithLongLong(ctx, 0);
    } else {
        if (RedisModule_ModuleTypeGetType(key) != TairHashType) {
            return RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        }
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    int field_expired = 0;
    int dbid = RedisModule_GetSelectedDb(ctx);
    if (fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, argv[2], 0)) {
        field_expired = 1;
    }

    TairHashVal *tairHashval = m_dictFetchValue(tair_hash_obj->hash, argv[2]);
    if (field_expired || tairHashval == NULL) {
        RedisModule_ReplyWithLongLong(ctx, 0);
    } else {
        RedisModule_ReplyWithLongLong(ctx, 1);
    }

    delEmptyTairHashIfNeeded(ctx, key, argv[1], tair_hash_obj);
    return REDISMODULE_OK;
}

/* EXHSTRLEN key field */
int TairHashTypeHstrlen_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc != 3) {
        return RedisModule_WrongArity(ctx);
    }

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        return RedisModule_ReplyWithLongLong(ctx, 0);
    } else {
        if (RedisModule_ModuleTypeGetType(key) != TairHashType) {
            return RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        }
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    int field_expired = 0;
    size_t len = 0;
    int dbid = RedisModule_GetSelectedDb(ctx);
    if (fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, argv[2], 0)) {
        field_expired = 1;
    }
    TairHashVal *val = m_dictFetchValue(tair_hash_obj->hash, argv[2]);
    if (field_expired || !val) {
        RedisModule_ReplyWithLongLong(ctx, 0);
    } else {
        RedisModule_StringPtrLen(val->value, &len);
        RedisModule_ReplyWithLongLong(ctx, len);
    }

    delEmptyTairHashIfNeeded(ctx, key, argv[1], tair_hash_obj);
    return REDISMODULE_OK;
}

/* EXHKEYS key */
int TairHashTypeHkeys_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc != 2) {
        return RedisModule_WrongArity(ctx);
    }

#if defined(SORT_MODE) || defined(SLAB_MODE)
    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
#else
    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ);
#endif
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        return RedisModule_ReplyWithArray(ctx, 0);
    } else {
        if (RedisModule_ModuleTypeGetType(key) != TairHashType) {
            return RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        }
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    TairHashVal *data;
    RedisModuleString *skey;
    uint64_t cn = 0;

    m_dictIterator *di;
    m_dictEntry *de;

    int dbid = RedisModule_GetSelectedDb(ctx);
    RedisModule_ReplyWithArray(ctx, REDISMODULE_POSTPONED_ARRAY_LEN);
    di = m_dictGetSafeIterator(tair_hash_obj->hash);
    while ((de = m_dictNext(di)) != NULL) {
        skey = (RedisModuleString *)dictGetKey(de);
#if defined(SORT_MODE) || defined(SLAB_MODE)
        data = (TairHashVal *)dictGetVal(de);
        if (isExpire(data->expire)) {
            continue;
        }
#else
        if (fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, skey, 0)) {
            continue;
        }
#endif
        RedisModule_ReplyWithString(ctx, skey);
        cn++;
    }
    m_dictReleaseIterator(di);

#if !defined(SORT_MODE) && !defined(SLAB_MODE)
    delEmptyTairHashIfNeeded(ctx, key, argv[1], tair_hash_obj);
#endif
    RedisModule_ReplySetArrayLength(ctx, cn);
    return REDISMODULE_OK;
}

/* EXHVALS key */
int TairHashTypeHvals_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc != 2) {
        return RedisModule_WrongArity(ctx);
    }
#if defined(SORT_MODE) || defined(SLAB_MODE)
    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
#else
    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ);
#endif
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        return RedisModule_ReplyWithArray(ctx, 0);
    } else {
        if (RedisModule_ModuleTypeGetType(key) != TairHashType) {
            return RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        }
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    RedisModuleString *skey;
    TairHashVal *data;
    uint64_t cn = 0;

    m_dictIterator *di;
    m_dictEntry *de;

    int dbid = RedisModule_GetSelectedDb(ctx);
    RedisModule_ReplyWithArray(ctx, REDISMODULE_POSTPONED_ARRAY_LEN);
    di = m_dictGetSafeIterator(tair_hash_obj->hash);
    while ((de = m_dictNext(di)) != NULL) {
        data = (TairHashVal *)dictGetVal(de);
#if defined(SORT_MODE) || defined(SLAB_MODE)
        if (isExpire(data->expire)) {
            continue;
        }
#else
        skey = (RedisModuleString *)dictGetKey(de);
        if (fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, skey, 0)) {
            continue;
        }
#endif
        RedisModule_ReplyWithString(ctx, data->value);
        cn++;
    }
    m_dictReleaseIterator(di);

#if !defined(SORT_MODE) && !defined(SLAB_MODE)
    delEmptyTairHashIfNeeded(ctx, key, argv[1], tair_hash_obj);
#endif
    RedisModule_ReplySetArrayLength(ctx, cn);
    return REDISMODULE_OK;
}

int tairHashGetAllGenericFunc(RedisModuleCtx *ctx, RedisModuleString **argv, int argc, int returnVer) {
    RedisModule_AutoMemory(ctx);

    if (argc != 2) {
        return RedisModule_WrongArity(ctx);
    }

#if defined(SORT_MODE) || defined(SLAB_MODE)
    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
#else
    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ);
#endif
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        RedisModule_ReplyWithArray(ctx, 0);
        return REDISMODULE_OK;
    } else {
        if (RedisModule_ModuleTypeGetType(key) != TairHashType) {
            return RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        }
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    TairHashVal *data;
    RedisModuleString *skey;
    uint64_t cn = 0;

    m_dictIterator *di;
    m_dictEntry *de;

    int dbid = RedisModule_GetSelectedDb(ctx);
    RedisModule_ReplyWithArray(ctx, REDISMODULE_POSTPONED_ARRAY_LEN);
    di = m_dictGetSafeIterator(tair_hash_obj->hash);
    while ((de = m_dictNext(di)) != NULL) {
        skey = (RedisModuleString *)dictGetKey(de);
        data = (TairHashVal *)dictGetVal(de);
#if defined(SORT_MODE) || defined(SLAB_MODE)
        if (isExpire(data->expire)) {
            continue;
        }
#else
        if (fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, skey, 0)) {
            continue;
        }
#endif
        RedisModule_ReplyWithString(ctx, skey);
        cn++;
        RedisModule_ReplyWithString(ctx, data->value);
        cn++;
        if (returnVer > 0) {
            RedisModule_ReplyWithLongLong(ctx, data->version);
            cn++;
        }
    }
    m_dictReleaseIterator(di);

#if !defined(SORT_MODE) && !defined(SLAB_MODE)
    delEmptyTairHashIfNeeded(ctx, key, argv[1], tair_hash_obj);
#endif
    RedisModule_ReplySetArrayLength(ctx, cn);
    return REDISMODULE_OK;
}

/* EXHGETALL key */
int TairHashTypeHgetAll_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    return tairHashGetAllGenericFunc(ctx, argv, argc, 0);
}

/* EXHGETALLWITHVER key */
int TairHashTypeHgetAllWithVer_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    return tairHashGetAllGenericFunc(ctx, argv, argc, 1);
}

static int parseScanCursor(RedisModuleString *cs, unsigned long *cursor) {
    char *eptr;

    /* Use strtoul() because we need an *unsigned* long, so
     * getLongLongFromObject() does not cover the whole cursor space. */
    errno = 0;
    size_t cs_len;
    const char *ptr = RedisModule_StringPtrLen(cs, &cs_len);
    *cursor = strtoul(ptr, &eptr, 10);
    if (isspace(ptr[0]) || eptr[0] != '\0' || errno == ERANGE) {
        return REDISMODULE_ERR;
    }
    return REDISMODULE_OK;
}

/* EXHSCAN key cursor [MATCH pattern] [COUNT count]*/
int TairHashTypeHscan_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc < 3 || argc > 7) {
        return RedisModule_WrongArity(ctx);
    }

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (REDISMODULE_KEYTYPE_EMPTY != type && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }

    unsigned long cursor;
    if (parseScanCursor(argv[2], &cursor) == REDISMODULE_ERR) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }

    /* Step 1: Parse options. */
    RedisModuleString *pattern = NULL;
    long long count = TAIR_HASH_SCAN_DEFAULT_COUNT;
    for (int j = 3; j < argc; j++) {
        RedisModuleString *next = (j == argc - 1) ? NULL : argv[j + 1];
        if (!mstrcasecmp(argv[j], "MATCH") && next) {
            pattern = next;
            j++;
        } else if (!mstrcasecmp(argv[j], "COUNT") && next) {
            if (RedisModule_StringToLongLong(next, &count) == REDISMODULE_ERR) {
                RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
                return REDISMODULE_ERR;
            }
            j++;
        } else {
            RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
            return REDISMODULE_ERR;
        }
    }

    tairHashObj *tair_hash_obj = NULL;
    if (type == REDISMODULE_KEYTYPE_EMPTY) {
        RedisModule_ReplyWithArray(ctx, 2);
        RedisModule_ReplyWithSimpleString(ctx, "0");
        RedisModule_ReplyWithArray(ctx, 0);
        return REDISMODULE_OK;
    } else {
        if (RedisModule_ModuleTypeGetType(key) != TairHashType) {
            return RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        }
        tair_hash_obj = RedisModule_ModuleTypeGetValue(key);
    }

    if (tair_hash_obj == NULL) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_ERR);
        return REDISMODULE_ERR;
    }

    /* Step 2: Iterate the collection.*/
    long maxiterations = count * 10;
    list *keys = m_listCreate();

    do {
        cursor = m_dictScan(tair_hash_obj->hash, cursor, tairhashScanCallback, NULL, keys);
    } while (cursor && maxiterations-- && listLength(keys) < (unsigned long)count);

    m_listNode *node, *nextnode;
    node = listFirst(keys);

    int dbid = RedisModule_GetSelectedDb(ctx);
    /* Step 3: Filter elements. */
    while (node) {
        RedisModuleString *skey = listNodeValue(node);
        nextnode = listNextNode(node);
        int filter = 0;

        /* Filter element if it does not match the pattern. */
        if (!filter && pattern) {
            if (!mstrmatchlen(pattern, skey, 0))
                filter = 1;
        }

        /* Filter element if it is an expired key. */
        if (!filter && fieldExpireIfNeeded(ctx, dbid, argv[1], tair_hash_obj, skey, 0)) {
            filter = 1;
        }

        /* Remove the element and its associted value if needed. */
        if (filter) {
            m_listDelNode(keys, node);
        }

        node = nextnode;
        nextnode = listNextNode(node);
        if (filter) {
            m_listDelNode(keys, node);
        }

        node = nextnode;
    }

    /* Step 4: Reply to the client. */
    RedisModule_ReplyWithArray(ctx, 2);
    RedisModule_ReplyWithString(ctx, RedisModule_CreateStringFromLongLong(ctx, cursor));

    RedisModule_ReplyWithArray(ctx, listLength(keys));
    while ((node = listFirst(keys)) != NULL) {
        RedisModuleString *skey = listNodeValue(node);
        RedisModule_ReplyWithString(ctx, skey);
        m_listDelNode(keys, node);
    }

    m_listRelease(keys);
    delEmptyTairHashIfNeeded(ctx, key, argv[1], tair_hash_obj);
    return REDISMODULE_OK;
}

/* exhexpireinfo */
int TairHashTypeActiveExpireInfo_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    REDISMODULE_NOT_USED(argv);

    RedisModule_AutoMemory(ctx);

    if (argc != 1) {
        return RedisModule_WrongArity(ctx);
    }

    RedisModuleString *info_a = RedisModule_CreateStringPrintf(
        ctx,
        "\r\n"
        "# Active expire statistics\r\n"
        "enable_active_expire:%ld\r\n"
        "tair_hash_active_expire_period:%ld\r\n"
        "tair_hash_active_expire_keys_per_loop:%ld\r\n"
        "tair_hash_active_expire_dbs_per_loop:%ld\r\n"
        "tair_hash_active_expire_last_time_msec:%ld\r\n"
        "tair_hash_active_expire_max_time_msec:%ld\r\n"
        "tair_hash_active_expire_avg_time_msec:%ld\r\n"
        "tair_hash_passive_expire_keys_per_loop:%ld\r\n",
        (long)g_expire_algorithm.enable_active_expire,
        (long)g_expire_algorithm.active_expire_period,
        (long)g_expire_algorithm.keys_per_active_loop,
        (long)g_expire_algorithm.dbs_per_active_loop,
        (long)g_expire_algorithm.stat_last_active_expire_time_msec,
        (long) g_expire_algorithm.stat_max_active_expire_time_msec,
        (long)g_expire_algorithm.stat_avg_active_expire_time_msec,
        (long)g_expire_algorithm.keys_per_passive_loop);

    size_t a_len, d_len, t_size = 0;
    const char *a_buf = RedisModule_StringPtrLen(info_a, &a_len);
    char buf[1024 * 1024] = {0};

    strncat(buf, a_buf, a_len);
    t_size += a_len;

#define DB_DETAIL "\r\n# DB detail statistics\r\n"

    d_len = strlen(DB_DETAIL);
    strncat(buf, DB_DETAIL, d_len);
    t_size += d_len;

    for (int i = 0; i < DB_NUM; ++i) {
        if (g_expire_algorithm.stat_active_expired_field[i] == 0 && g_expire_algorithm.stat_passive_expired_field[i] == 0) {
            continue;
        }
        RedisModuleString *info_d = RedisModule_CreateStringPrintf(ctx, "db: %d, active_expired_fields: %ld, passive_expired_fields: %ld\r\n", i,
                                                                   (long)g_expire_algorithm.stat_active_expired_field[i], (long)g_expire_algorithm.stat_passive_expired_field[i]);
        const char *d_buf = RedisModule_StringPtrLen(info_d, &d_len);
        strncat(buf, d_buf, d_len);
        RedisModule_FreeString(ctx, info_d);
        t_size += d_len;
    }
    RedisModule_ReplyWithStringBuffer(ctx, buf, t_size);
    RedisModule_FreeString(ctx, info_a);
    return REDISMODULE_OK;
}

/* ===================== Last-write-wins writes and source sync ===================== */

/* Byte-wise order with a shorter prefix sorting first: the order Cassandra/Scylla use
 * to break write-timestamp ties between cell values. */
static int compareValues(RedisModuleString *a, RedisModuleString *b) {
    size_t alen, blen;
    const char *aptr = RedisModule_StringPtrLen(a, &alen);
    const char *bptr = RedisModule_StringPtrLen(b, &blen);
    int cmp = memcmp(aptr, bptr, alen < blen ? alen : blen);
    if (cmp != 0) {
        return cmp;
    }
    return (alen > blen) - (alen < blen);
}

/* Rejects the command before it changes anything unless every version in the
 * <field> <value> <version> triples of argv[start..argc) is a positive integer. */
static int validateLwwTriples(RedisModuleCtx *ctx, RedisModuleString **argv, int start, int argc) {
    long long version;
    for (int i = start; i < argc; i += 3) {
        if (RedisModule_StringToLongLong(argv[i + 2], &version) != REDISMODULE_OK || version <= 0) {
            RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
            return REDISMODULE_ERR;
        }
    }
    return REDISMODULE_OK;
}

/* Returns the tairhash at an open key, creating it when the key is empty, or NULL
 * (after replying WRONGTYPE) when the key holds another type. */
static tairHashObj *getOrCreateTairHash(RedisModuleCtx *ctx, RedisModuleKey *key, RedisModuleString *keyname) {
    if (RedisModule_KeyType(key) == REDISMODULE_KEYTYPE_EMPTY) {
        tairHashObj *o = createTairHashTypeObject();
        o->key = RedisModule_CreateStringFromString(NULL, keyname);
        RedisModule_ModuleTypeSetValue(key, TairHashType, o);
        return o;
    }
    if (RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return NULL;
    }
    return RedisModule_ModuleTypeGetValue(key);
}

/* Applies one last-write-wins write and returns 1 if it landed: the field is absent,
 * the version is higher, or the versions tie and the value is byte-wise greater. The
 * tie-break makes the result independent of arrival order. A landed write clears the
 * field's TTL, as EXHSET does, and is added to `batch` for replication. */
static int lwwApply(RedisModuleCtx *ctx, int dbid, RedisModuleString *key, tairHashObj *o, RedisModuleString *field, RedisModuleString *value,
                    RedisModuleString *version_str, long long version, long long now, FieldStateBatch *batch) {
    TairHashVal *val = m_dictFetchValue(o->hash, field);
    if (val != NULL && val->expire != 0 && fieldExpireIfNeeded(ctx, dbid, key, o, field, 0)) {
        /* Gone on a master; a writable replica keeps it until the master's EXHDEL. */
        val = m_dictFetchValue(o->hash, field);
    }

    int exists = val != NULL;
    if (exists) {
        if (version < val->version || (version == val->version && compareValues(value, val->value) <= 0)) {
            return 0;
        }
        if (val->expire) {
            g_expire_algorithm.delete(ctx, dbid, key, o, field, val->expire);
            val->expire = 0;
        }
        RedisModule_FreeString(NULL, val->value);
    } else {
        val = createTairHashVal();
    }

    val->value = takeAndRef(value);
    val->version = version;
    val->seq = tairHashNextSeq(now);
    if (!exists) {
        m_dictAdd(o->hash, takeAndRef(field), val);
    }
    fieldStateBatchAdd(batch, field, val, version_str);
    return 1;
}

/* EXHMSETLWW <key> <field> <value> <version> [<field> <value> <version> ...]
 *
 * Reply: one integer per triple, 1 if the write landed and 0 if the field already held
 * a newer (or tie-winning) write. A losing write is an expected outcome of
 * last-write-wins, not an error, so it never fails a pipeline or a MULTI. */
int TairHashTypeHmsetLww_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc < 5 || (argc - 2) % 3 != 0) {
        return RedisModule_WrongArity(ctx);
    }

    /* Read before any key or field lookup: the clock read serializes the pipeline
     * (ISB on arm64, lfence/rdtscp on x86), stalling on any cache miss still in flight. */
    long long now = seqClockNow();
    if (validateLwwTriples(ctx, argv, 2, argc) != REDISMODULE_OK) {
        return REDISMODULE_ERR;
    }

    int dbid = RedisModule_GetSelectedDb(ctx);
    g_expire_algorithm.passiveExpire(ctx, dbid, argv[1]);

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    tairHashObj *o = getOrCreateTairHash(ctx, key, argv[1]);
    if (o == NULL) {
        return REDISMODULE_ERR;
    }

    long long version;
    FieldStateBatch batch;
    fieldStateBatchInit(&batch, ctx, argv[1], (argc - 2) / 3);
    RedisModule_ReplyWithArray(ctx, (argc - 2) / 3);
    for (int i = 2; i < argc; i += 3) {
        RedisModule_StringToLongLong(argv[i + 2], &version);
        RedisModule_ReplyWithLongLong(ctx, lwwApply(ctx, dbid, argv[1], o, argv[i], argv[i + 1], argv[i + 2], version, now, &batch));
    }
    fieldStateBatchFlush(&batch);
    return REDISMODULE_OK;
}

/* EXHSYNCGET <key> [<field> ...]
 *
 * Reply: [synced, token, [field, value, version, ...]]. Entries are returned only when
 * the hash is synced (an authoritative copy, see EXHSYNC); otherwise the caller must
 * read the source of truth and hand it to EXHSYNC together with `token`. With fields
 * given, only those that exist are returned. */
int TairHashTypeSyncGet_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc < 2) {
        return RedisModule_WrongArity(ctx);
    }

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    int type = RedisModule_KeyType(key);
    if (type != REDISMODULE_KEYTYPE_EMPTY && RedisModule_ModuleTypeGetType(key) != TairHashType) {
        RedisModule_ReplyWithError(ctx, REDISMODULE_ERRORMSG_WRONGTYPE);
        return REDISMODULE_ERR;
    }
    tairHashObj *o = type == REDISMODULE_KEYTYPE_EMPTY ? NULL : RedisModule_ModuleTypeGetValue(key);

    /* Every write after this point is assigned a sequence above the token. */
    long long token = g_last_seq;

    RedisModule_ReplyWithArray(ctx, 3);
    if (o == NULL || !o->synced) {
        RedisModule_ReplyWithLongLong(ctx, 0);
        RedisModule_ReplyWithLongLong(ctx, token);
        RedisModule_ReplyWithArray(ctx, 0);
        return REDISMODULE_OK;
    }
    RedisModule_ReplyWithLongLong(ctx, 1);
    RedisModule_ReplyWithLongLong(ctx, token);

    int dbid = RedisModule_GetSelectedDb(ctx);
    long len = 0;
    TairHashVal *val;
    RedisModule_ReplyWithArray(ctx, REDISMODULE_POSTPONED_ARRAY_LEN);
    if (argc == 2) {
        m_dictIterator *di = m_dictGetSafeIterator(o->hash);
        m_dictEntry *de;
        while ((de = m_dictNext(di)) != NULL) {
            RedisModuleString *field = dictGetKey(de);
            val = dictGetVal(de);
#if defined(SORT_MODE) || defined(SLAB_MODE)
            if (isExpire(val->expire)) {
                continue;
            }
#else
            if (fieldExpireIfNeeded(ctx, dbid, argv[1], o, field, 0)) {
                continue;
            }
#endif
            RedisModule_ReplyWithString(ctx, field);
            RedisModule_ReplyWithString(ctx, val->value);
            RedisModule_ReplyWithLongLong(ctx, val->version);
            len += 3;
        }
        m_dictReleaseIterator(di);
    } else {
        for (int i = 2; i < argc; i++) {
            if (fieldExpireIfNeeded(ctx, dbid, argv[1], o, argv[i], 0)) {
                continue;
            }
            val = m_dictFetchValue(o->hash, argv[i]);
            if (val == NULL) {
                continue;
            }
            RedisModule_ReplyWithString(ctx, argv[i]);
            RedisModule_ReplyWithString(ctx, val->value);
            RedisModule_ReplyWithLongLong(ctx, val->version);
            len += 3;
        }
    }
    RedisModule_ReplySetArrayLength(ctx, len);
    return REDISMODULE_OK;
}

/* Membership set over the snapshot's field names; argv owns the strings. */
static m_dictType snapshotFieldsDictType = {
    dictModuleStrHash,       /* hash function */
    NULL,                    /* key dup */
    NULL,                    /* val dup */
    dictModuleStrKeyCompare, /* key compare */
    NULL,                    /* key destructor */
    NULL                     /* val destructor */
};

/* EXHSYNC <key> <token> [<field> <value> <version> ...]
 *
 * Reconciles the hash with a full snapshot of the source of truth read after
 * EXHSYNCGET returned `token`: snapshot fields are merged with last-write-wins
 * (EXHMSETLWW semantics), fields absent from the snapshot are removed unless they were
 * written after `token` (those raced the snapshot read and must survive regardless of
 * their version), and the hash is marked synced. An empty snapshot leaves an empty,
 * synced hash: "the source has nothing" is a cacheable answer.
 *
 * Reply: [applied, removed]. */
int TairHashTypeSync_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc < 3 || (argc - 3) % 3 != 0) {
        return RedisModule_WrongArity(ctx);
    }

    /* Read before any key or field lookup: the clock read serializes the pipeline
     * (ISB on arm64, lfence/rdtscp on x86), stalling on any cache miss still in flight. */
    long long now = seqClockNow();

    long long token;
    if (RedisModule_StringToLongLong(argv[2], &token) != REDISMODULE_OK || token < 0) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
        return REDISMODULE_ERR;
    }
    if (validateLwwTriples(ctx, argv, 3, argc) != REDISMODULE_OK) {
        return REDISMODULE_ERR;
    }

    int dbid = RedisModule_GetSelectedDb(ctx);
    g_expire_algorithm.passiveExpire(ctx, dbid, argv[1]);

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    tairHashObj *o = getOrCreateTairHash(ctx, key, argv[1]);
    if (o == NULL) {
        return REDISMODULE_ERR;
    }

    long long applied = 0, removed = 0, version;
    dict *snapshot = m_dictCreate(&snapshotFieldsDictType, NULL);
    FieldStateBatch batch;
    fieldStateBatchInit(&batch, ctx, argv[1], (argc - 3) / 3);
    for (int i = 3; i < argc; i += 3) {
        RedisModule_StringToLongLong(argv[i + 2], &version);
        applied += lwwApply(ctx, dbid, argv[1], o, argv[i], argv[i + 1], argv[i + 2], version, now, &batch);
        m_dictAdd(snapshot, argv[i], NULL);
    }
    fieldStateBatchFlush(&batch);

    /* Every snapshot field is present after the merge, so only a larger hash can hold
     * fields the source no longer has; they are propagated as one EXHDEL. */
    if (dictSize(o->hash) > dictSize(snapshot)) {
        size_t cap = 1 + dictSize(o->hash) - dictSize(snapshot), ndel = 0;
        RedisModuleString **del = RedisModule_Alloc(sizeof(*del) * cap);
        del[ndel++] = argv[1];
        m_dictIterator *di = m_dictGetSafeIterator(o->hash);
        m_dictEntry *de;
        while ((de = m_dictNext(di)) != NULL) {
            RedisModuleString *field = dictGetKey(de);
            TairHashVal *val = dictGetVal(de);
            if (val->seq > token || m_dictFind(snapshot, field) != NULL) {
                continue;
            }
            if (val->expire) {
                g_expire_algorithm.delete(ctx, dbid, argv[1], o, field, val->expire);
            }
            /* Outlives the dict entry deleted below. */
            del[ndel++] = takeAndRef(field);
            m_dictDelete(o->hash, field);
            removed++;
        }
        m_dictReleaseIterator(di);
        if (ndel > 1) {
            RedisModule_Replicate(ctx, "EXHDEL", "v", del, ndel);
        }
        for (size_t i = 1; i < ndel; i++) {
            RedisModule_FreeString(NULL, del[i]);
        }
        RedisModule_Free(del);
    }
    m_dictRelease(snapshot);

    if (!o->synced) {
        o->synced = 1;
        RedisModule_Replicate(ctx, "EXHSYNCREPL", "s", argv[1]);
    }

    RedisModule_ReplyWithArray(ctx, 2);
    RedisModule_ReplyWithLongLong(ctx, applied);
    RedisModule_ReplyWithLongLong(ctx, removed);
    return REDISMODULE_OK;
}

/* EXHSYNCREPL <key>
 *
 * Internal: marks <key> synced, creating an empty hash if needed. Emitted by EXHSYNC
 * for replicas and the AOF, and by AOF rewrite; rejected from regular clients. */
int TairHashTypeSyncRepl_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc != 2) {
        return RedisModule_WrongArity(ctx);
    }
    if (!mayRestoreState(ctx)) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_CMD);
        return REDISMODULE_ERR;
    }

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    tairHashObj *o = getOrCreateTairHash(ctx, key, argv[1]);
    if (o == NULL) {
        return REDISMODULE_ERR;
    }
    o->synced = 1;
    RedisModule_ReplicateVerbatim(ctx);
    RedisModule_ReplyWithSimpleString(ctx, "OK");
    return REDISMODULE_OK;
}

/* EXHMSETREPL <key> <field> <value> <version> <expire> <seq> [...]
 *
 * Internal: sets each field to exactly the given state (expire is an absolute unix time
 * in milliseconds, 0 for none), creating the hash if needed. Emitted by every write
 * command for replicas and the AOF, and by AOF rewrite; rejected from regular clients.
 * Version 0 is accepted: fields written before versioning existed carry it. */
int TairHashTypeHmsetRepl_RedisCommand(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    RedisModule_AutoMemory(ctx);

    if (argc < 2 + FIELD_STATE_ARGS || (argc - 2) % FIELD_STATE_ARGS != 0) {
        return RedisModule_WrongArity(ctx);
    }
    if (!mayRestoreState(ctx)) {
        RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_INTERNAL_CMD);
        return REDISMODULE_ERR;
    }

    long long version, expire, seq;
    for (int i = 2; i < argc; i += FIELD_STATE_ARGS) {
        if (RedisModule_StringToLongLong(argv[i + 2], &version) != REDISMODULE_OK || version < 0 ||
            RedisModule_StringToLongLong(argv[i + 3], &expire) != REDISMODULE_OK || expire < 0 ||
            RedisModule_StringToLongLong(argv[i + 4], &seq) != REDISMODULE_OK || seq < 0) {
            RedisModule_ReplyWithError(ctx, TAIRHASH_ERRORMSG_SYNTAX);
            return REDISMODULE_ERR;
        }
    }

    RedisModuleKey *key = RedisModule_OpenKey(ctx, argv[1], REDISMODULE_READ | REDISMODULE_WRITE);
    tairHashObj *o = getOrCreateTairHash(ctx, key, argv[1]);
    if (o == NULL) {
        return REDISMODULE_ERR;
    }

    int dbid = RedisModule_GetSelectedDb(ctx);
    for (int i = 2; i < argc; i += FIELD_STATE_ARGS) {
        RedisModuleString *field = argv[i];
        RedisModule_StringToLongLong(argv[i + 2], &version);
        RedisModule_StringToLongLong(argv[i + 3], &expire);
        RedisModule_StringToLongLong(argv[i + 4], &seq);

        TairHashVal *val = m_dictFetchValue(o->hash, field);
        int exists = val != NULL;
        if (exists) {
            if (val->expire) {
                g_expire_algorithm.delete(ctx, dbid, argv[1], o, field, val->expire);
                val->expire = 0;
            }
            RedisModule_FreeString(NULL, val->value);
        } else {
            val = createTairHashVal();
        }
        val->value = takeAndRef(argv[i + 1]);
        val->version = version;
        val->seq = seq;
        tairHashObserveSeq(seq);
        if (!exists) {
            m_dictAdd(o->hash, takeAndRef(field), val);
        }
        if (expire) {
            g_expire_algorithm.insert(ctx, dbid, argv[1], o, field, expire);
            val->expire = expire;
        }
    }

    RedisModule_ReplicateVerbatim(ctx);
    RedisModule_ReplyWithSimpleString(ctx, "OK");
    return REDISMODULE_OK;
}

/* ========================== "tairhashtype" type methods ======================= */

/*
 * RDB encoding versions:
 *   0: <len> <key> then per field <field> <version> <expire> <value>
 *   1: <len> <key> <synced> then per field <field> <version> <expire> <value> <seq>
 * Version 0 data loads as unsynced with seq 0 (older than any sync token).
 */
#define TAIRHASH_ENCVER 1

void *TairHashTypeRdbLoad(RedisModuleIO *rdb, int encver) {
    if (encver > TAIRHASH_ENCVER) {
        RedisModule_LogIOError(rdb, "warning", "tairhash: unsupported encoding version %d (max %d)", encver, TAIRHASH_ENCVER);
        return NULL;
    }

    tairHashObj *o = createTairHashTypeObject();
    uint64_t len = RedisModule_LoadUnsigned(rdb);
    o->key = RedisModule_LoadString(rdb);
    if (encver >= 1) {
        o->synced = RedisModule_LoadUnsigned(rdb) != 0;
    }

    int dbid = RedisModule_GetDbIdFromIO ? RedisModule_GetDbIdFromIO(rdb) : 0;

    RedisModuleString *skey;
    long long version, expire;
    RedisModuleString *value;

    while (len--) {
        skey = RedisModule_LoadString(rdb);
        version = RedisModule_LoadUnsigned(rdb);
        expire = RedisModule_LoadUnsigned(rdb);
        value = RedisModule_LoadString(rdb);
        TairHashVal *hashv = createTairHashVal();
        hashv->version = version;
        hashv->expire = expire;
        hashv->value = takeAndRef(value);
        if (encver >= 1) {
            hashv->seq = RedisModule_LoadUnsigned(rdb);
            tairHashObserveSeq(hashv->seq);
        }
        m_dictAdd(o->hash, takeAndRef(skey), hashv);
        if (hashv->expire) {
            g_expire_algorithm.insert(NULL, dbid, NULL, o, skey, hashv->expire);
        }
        RedisModule_FreeString(NULL, value);
        RedisModule_FreeString(NULL, skey);
    }

    return o;
}

void TairHashTypeRdbSave(RedisModuleIO *rdb, void *value) {
    tairHashObj *o = (tairHashObj *)value;
    RedisModuleString *skey;

    m_dictIterator *di;
    m_dictEntry *de;

    RedisModule_SaveUnsigned(rdb, dictSize(o->hash));
    RedisModule_SaveString(rdb, o->key);
    RedisModule_SaveUnsigned(rdb, o->synced);

    di = m_dictGetIterator(o->hash);
    while ((de = m_dictNext(di)) != NULL) {
        skey = (RedisModuleString *)dictGetKey(de);
        TairHashVal *val = (TairHashVal *)dictGetVal(de);
        RedisModule_SaveString(rdb, skey);
        RedisModule_SaveUnsigned(rdb, val->version);
        RedisModule_SaveUnsigned(rdb, val->expire);
        RedisModule_SaveString(rdb, val->value);
        RedisModule_SaveUnsigned(rdb, val->seq);
    }
    m_dictReleaseIterator(di);
}

/* Fields per EXHMSETREPL emitted by AOF rewrite: one command per field made the AOF
 * large and its replay slow, an unbounded one would make a huge hash a huge command. */
#define AOF_REWRITE_FIELDS_PER_CMD 64

static void emitFieldStates(RedisModuleIO *aof, RedisModuleString **argv, int argc) {
    RedisModule_EmitAOF(aof, "EXHMSETREPL", "v", argv, (size_t)argc);
    /* The numeric arguments of each field are owned here; field and value are not. */
    for (int i = 1; i < argc; i += FIELD_STATE_ARGS) {
        for (int j = 2; j < FIELD_STATE_ARGS; j++) {
            if (argv[i + j] != g_zero_str) {
                RedisModule_FreeString(NULL, argv[i + j]);
            }
        }
    }
}

void TairHashTypeAofRewrite(RedisModuleIO *aof, RedisModuleString *key, void *value) {
    tairHashObj *o = (tairHashObj *)value;
    RedisModuleString *argv[1 + FIELD_STATE_ARGS * AOF_REWRITE_FIELDS_PER_CMD];
    int argc = 1;
    /* The server passes a stack-allocated key, which the "v" format would try to retain. */
    argv[0] = RedisModule_CreateStringFromString(NULL, key);

    /* First, so a synced hash without fields is recreated too. */
    if (o->synced) {
        RedisModule_EmitAOF(aof, "EXHSYNCREPL", "s", key);
    }

    m_dictIterator *di = m_dictGetIterator(o->hash);
    m_dictEntry *de;
    while ((de = m_dictNext(di)) != NULL) {
        TairHashVal *val = (TairHashVal *)dictGetVal(de);
        if (isExpire(val->expire)) {
            /* For expired field, we do not REWRITE it. */
            continue;
        }
        argv[argc++] = (RedisModuleString *)dictGetKey(de);
        argv[argc++] = val->value;
        argv[argc++] = RedisModule_CreateStringFromLongLong(NULL, val->version);
        argv[argc++] = val->expire ? RedisModule_CreateStringFromLongLong(NULL, val->expire) : g_zero_str;
        argv[argc++] = RedisModule_CreateStringFromLongLong(NULL, val->seq);
        if (argc == (int)(sizeof(argv) / sizeof(argv[0]))) {
            emitFieldStates(aof, argv, argc);
            argc = 1;
        }
    }
    m_dictReleaseIterator(di);
    if (argc > 1) {
        emitFieldStates(aof, argv, argc);
    }
    RedisModule_FreeString(NULL, argv[0]);
}

void TairHashTypeFree(void *value) {
    if (value) {
        tairHashTypeReleaseObject(value);
    }
}

/* Set at load time when the server provides RedisModule_MallocSizeString (Redis 7.0+). */
static size_t (*g_malloc_size_string)(RedisModuleString *str) = NULL;

/* Allocator size of a module string. Without RedisModule_MallocSizeString, a string
 * longer than the 44 bytes Redis embeds in the object header has its payload in a
 * separate allocation that only its length can approximate. */
static size_t stringMallocSize(RedisModuleString *str) {
    if (g_malloc_size_string) {
        return g_malloc_size_string(str);
    }
    size_t len;
    RedisModule_StringPtrLen(str, &len);
    return RedisModule_MallocSize(str) + (len > 44 ? len + 1 : 0);
}

/* What the hash actually holds in the allocator: object, dict and bucket tables,
 * entries, field and value strings and the expire index. Skiplist members share the
 * dict's field strings and are not counted twice. */
static size_t tairHashMemUsage(const tairHashObj *o) {
    size_t size = RedisModule_MallocSize((void *)o) + RedisModule_MallocSize(o->hash);
    for (int t = 0; t < 2; t++) {
        if (o->hash->ht[t].table) {
            size += RedisModule_MallocSize(o->hash->ht[t].table);
        }
    }
    if (o->key) {
        size += stringMallocSize(o->key);
    }

    m_dictIterator *di = m_dictGetIterator(o->hash);
    m_dictEntry *de;
    while ((de = m_dictNext(di)) != NULL) {
        TairHashVal *val = (TairHashVal *)dictGetVal(de);
        size += RedisModule_MallocSize(de) + stringMallocSize(dictGetKey(de)) + RedisModule_MallocSize(val) + stringMallocSize(val->value);
    }
    m_dictReleaseIterator(di);

    if (o->expire_index) {
        size += RedisModule_MallocSize(o->expire_index);
#ifdef SLAB_MODE
        for (tairhash_zskiplistNode *x = o->expire_index->header; x != NULL; x = x->level[0].forward) {
            size += RedisModule_MallocSize(x) + (x->slab ? RedisModule_MallocSize(x->slab) : 0);
        }
#else
        for (m_zskiplistNode *x = o->expire_index->header; x != NULL; x = x->level[0].forward) {
            size += RedisModule_MallocSize(x);
        }
#endif
    }
    return size;
}

#if defined(SORT_MODE) || defined(SLAB_MODE)

size_t TairHashTypeMemUsage2(RedisModuleKeyOptCtx *ctx, const void *value) {
    REDISMODULE_NOT_USED(ctx);
    return value ? tairHashMemUsage((const tairHashObj *)value) : 0;
}

void TairHashTypeUnlink2(RedisModuleKeyOptCtx *ctx, const void *value) {
    struct tairHashObj *o = (struct tairHashObj *)value;

    int dbid = RedisModule_GetDbIdFromOptCtx(ctx);

    if (TAIRHASH_EXPIRE_INDEX_LEN(o)) {
        /* UNLINK is a synchronous call, so ExpireNode can be safely deleted here. */
#ifdef SLAB_MODE
        m_zslDelete(g_expire_index[dbid], o->expire_index->header->level[0].forward->expire_min, o->key, NULL);
#else
        m_zslDelete(g_expire_index[dbid], o->expire_index->header->level[0].forward->score, o->key, NULL);
#endif
    }
}

void *TairHashTypeCopy2(RedisModuleKeyOptCtx *ctx, const void *value) {
    struct tairHashObj *old = (struct tairHashObj *)value;
    struct tairHashObj *new = createTairHashTypeObject();

    int to_dbid = RedisModule_GetToDbIdFromOptCtx(ctx);
    const RedisModuleString *tokey = RedisModule_GetToKeyNameFromOptCtx(ctx);

    new->key = RedisModule_CreateStringFromString(NULL, tokey);
    new->synced = old->synced;
    m_dictExpand(new->hash, dictSize(old->hash));

    /* Copy hash. */
    m_dictIterator *di;
    m_dictEntry *de;
    RedisModuleString *field;
    di = m_dictGetIterator(old->hash);
    while ((de = m_dictNext(di)) != NULL) {
        field = RedisModule_CreateStringFromString(NULL, (RedisModuleString *)dictGetKey(de));
        TairHashVal *oldval = (TairHashVal *)dictGetVal(de);
        TairHashVal *newval = createTairHashVal();
        newval->expire = oldval->expire;
        newval->version = oldval->version;
        newval->seq = oldval->seq;
        newval->value = RedisModule_CreateStringFromString(NULL, oldval->value);
        m_dictAdd(new->hash, field, newval);
        if (newval->expire) {
            g_expire_algorithm.insert(NULL, to_dbid, NULL, new, field, newval->expire);
        }
    }
    m_dictReleaseIterator(di);
    return new;
}

size_t TairHashTypeEffort2(RedisModuleKeyOptCtx *ctx, const void *value) {
    tairHashObj *o = (tairHashObj *)value;
    return dictSize(o->hash) + TAIRHASH_EXPIRE_INDEX_LEN(o);
}
#else

size_t TairHashTypeMemUsage(const void *value) {
    return value ? tairHashMemUsage((const tairHashObj *)value) : 0;
}

size_t TairHashTypeEffort(RedisModuleString *key, const void *value) {
    REDISMODULE_NOT_USED(key);
    tairHashObj *o = (tairHashObj *)value;
    return dictSize(o->hash) + TAIRHASH_EXPIRE_INDEX_LEN(o);
}

#endif

void TairHashTypeDigest(RedisModuleDigest *md, void *value) {
    tairHashObj *o = (tairHashObj *)value;

    RedisModuleString *skey;

    if (!o) {
        return;
    }

    m_dictIterator *di;
    m_dictEntry *de;

    /* Everything replication and persistence must reproduce, so DEBUG DIGEST catches a
     * replica or a reload that diverged in version, TTL or write sequence too. */
    di = m_dictGetIterator(o->hash);
    while ((de = m_dictNext(di)) != NULL) {
        TairHashVal *val = (TairHashVal *)dictGetVal(de);
        skey = (RedisModuleString *)dictGetKey(de);
        size_t val_len, skey_len;
        const char *val_ptr = RedisModule_StringPtrLen(val->value, &val_len);
        const char *skey_ptr = RedisModule_StringPtrLen(skey, &skey_len);
        RedisModule_DigestAddStringBuffer(md, (unsigned char *)skey_ptr, skey_len);
        RedisModule_DigestAddStringBuffer(md, (unsigned char *)val_ptr, val_len);
        RedisModule_DigestAddLongLong(md, val->version);
        RedisModule_DigestAddLongLong(md, val->expire);
        RedisModule_DigestAddLongLong(md, val->seq);
        RedisModule_DigestEndSequence(md);
    }
    m_dictReleaseIterator(di);

    RedisModule_DigestAddLongLong(md, o->synced);
    RedisModule_DigestEndSequence(md);
}

int Module_CreateCommands(RedisModuleCtx *ctx) {
#define CREATE_CMD(name, tgt, attr, firstkey, lastkey, keystep)                                              \
    do {                                                                                                     \
        if (RedisModule_CreateCommand(ctx, name, tgt, attr, firstkey, lastkey, keystep) != REDISMODULE_OK) { \
            RedisModule_Log(ctx, "notice", "reg cmd error");                                                 \
            return REDISMODULE_ERR;                                                                          \
        }                                                                                                    \
    } while (0);

#define CREATE_WRCMD(name, tgt) CREATE_CMD(name, tgt, "write deny-oom", 1, 1, 1);
#define CREATE_ROCMD(name, tgt) CREATE_CMD(name, tgt, "readonly fast", 1, 1, 1);
#define CREATE_WRMCMD(name, tgt, firstkey, lastkey, keystep) CREATE_CMD(name, tgt, "write deny-oom", firstkey, lastkey, keystep);
#define CREATE_ROMCMD(name, tgt, firstkey, lastkey, keystep) CREATE_CMD(name, tgt, "readonly fast", firstkey, lastkey, keystep);

    /* write cmds */
    CREATE_WRCMD("exhset", TairHashTypeHset_RedisCommand)
    CREATE_WRCMD("exhdel", TairHashTypeHdel_RedisCommand)
    CREATE_WRCMD("exhdelrepl", TairHashTypeHdelRepl_RedisCommand)
    CREATE_WRCMD("exhdelwithver", TairHashTypeHdelWithVer_RedisCommand)
    CREATE_WRCMD("exhincrby", TairHashTypeHincrBy_RedisCommand)
    CREATE_WRCMD("exhincrbyfloat", TairHashTypeHincrByFloat_RedisCommand)
    CREATE_WRCMD("exhsetnx", TairHashTypeHsetNx_RedisCommand)
    CREATE_WRCMD("exhmset", TairHashTypeHmset_RedisCommand)
    CREATE_WRCMD("exhmsetwithopts", TairHashTypeHmsetWithOpts_RedisCommand)
    CREATE_WRCMD("exhsetver", TairHashTypeHsetVer_RedisCommand)
    CREATE_WRCMD("exhexpire", TairHashTypeHexpire_RedisCommand)
    CREATE_WRCMD("exhexpireat", TairHashTypeHexpireAt_RedisCommand)
    CREATE_WRCMD("exhpexpire", TairHashTypeHpexpire_RedisCommand)
    CREATE_WRCMD("exhpexpireat", TairHashTypeHpexpireAt_RedisCommand)
    CREATE_WRCMD("exhpersist", TairHashTypeHpersist_RedisCommand)
    CREATE_WRCMD("exhmsetlww", TairHashTypeHmsetLww_RedisCommand)
    CREATE_WRCMD("exhsync", TairHashTypeSync_RedisCommand)
    CREATE_WRCMD("exhsyncrepl", TairHashTypeSyncRepl_RedisCommand)
    CREATE_WRCMD("exhmsetrepl", TairHashTypeHmsetRepl_RedisCommand)

    /* readonly cmds */
    CREATE_ROCMD("exhget", TairHashTypeHget_RedisCommand)
    CREATE_ROCMD("exhlen", TairHashTypeHlen_RedisCommand)
    CREATE_ROCMD("exhexists", TairHashTypeHexists_RedisCommand)
    CREATE_ROCMD("exhstrlen", TairHashTypeHstrlen_RedisCommand)
    CREATE_ROCMD("exhkeys", TairHashTypeHkeys_RedisCommand)
    CREATE_ROCMD("exhvals", TairHashTypeHvals_RedisCommand)
    CREATE_ROCMD("exhgetall", TairHashTypeHgetAll_RedisCommand)
    CREATE_ROCMD("exhgetallwithver", TairHashTypeHgetAllWithVer_RedisCommand)
    CREATE_ROCMD("exhmget", TairHashTypeHmget_RedisCommand)
    CREATE_ROCMD("exhmgetwithver", TairHashTypeHmgetWithVer_RedisCommand)
    CREATE_ROCMD("exhscan", TairHashTypeHscan_RedisCommand)
    CREATE_ROCMD("exhver", TairHashTypeHver_RedisCommand)
    CREATE_ROCMD("exhttl", TairHashTypeHttl_RedisCommand)
    CREATE_ROCMD("exhpttl", TairHashTypeHpttl_RedisCommand)
    CREATE_ROCMD("exhgetwithver", TairHashTypeHgetWithVer_RedisCommand)
    CREATE_ROMCMD("exhexpireinfo", TairHashTypeActiveExpireInfo_RedisCommand, 0, 0, 0)
    /* O(N) without a field list, so not "fast". */
    CREATE_CMD("exhsyncget", TairHashTypeSyncGet_RedisCommand, "readonly", 1, 1, 1)

    return REDISMODULE_OK;
}

int __attribute__((visibility("default"))) RedisModule_OnLoad(RedisModuleCtx *ctx, RedisModuleString **argv, int argc) {
    REDISMODULE_NOT_USED(argv);
    REDISMODULE_NOT_USED(argc);

    if (RedisModule_Init(ctx, "tairhash", 1, REDISMODULE_APIVER_1) == REDISMODULE_ERR)
        return REDISMODULE_ERR;

    if (argc % 2) {
        RedisModule_Log(ctx, "warning", "Invalid number of arguments passed");
        return REDISMODULE_ERR;
    }

    if (RedisModule_GetServerVersion) {
        int version = RedisModule_GetServerVersion();
        redis_patch_ver = version & 0x000000ff;
        redis_minor_ver = (version & 0x0000ff00) >> 8;
        redis_major_ver = (version & 0x00ff0000) >> 16;
    }

#if defined(SORT_MODE) || defined(SLAB_MODE)
    if (redis_major_ver < 7) {
        RedisModule_Log(ctx, "warning", "Redis version (%d.%d.%d) is too old, please upgrade to 7.0.0 or above", redis_major_ver, redis_minor_ver, redis_patch_ver);
        return REDISMODULE_ERR;
    }
#endif

    g_expire_algorithm.enable_active_expire = 1;
    g_expire_algorithm.active_expire_period = TAIR_HASH_ACTIVE_EXPIRE_PERIOD;
    g_expire_algorithm.dbs_per_active_loop = TAIR_HASH_ACTIVE_DBS_PER_CALL;
    g_expire_algorithm.keys_per_active_loop = TAIR_HASH_ACTIVE_EXPIRE_KEYS_PER_LOOP;
    g_expire_algorithm.keys_per_passive_loop = TAIR_HASH_PASSIVE_EXPIRE_KEYS_PER_LOOP;

    for (int ii = 0; ii < argc; ii += 2) {
        if (!mstrcasecmp(argv[ii], "enable_active_expire")) {
            long long v;
            if (RedisModule_StringToLongLong(argv[ii + 1], &v) == REDISMODULE_ERR) {
                RedisModule_Log(ctx, "warning", "Invalid argument for enable_active_expire");
                return REDISMODULE_ERR;
            }
            g_expire_algorithm.enable_active_expire = v;
        } else if (!mstrcasecmp(argv[ii], "active_expire_period")) {
            long long v;
            if (RedisModule_StringToLongLong(argv[ii + 1], &v) == REDISMODULE_ERR) {
                RedisModule_Log(ctx, "warning", "Invalid argument for active_expire_period");
                return REDISMODULE_ERR;
            }
            g_expire_algorithm.active_expire_period = v;
        } else if (!mstrcasecmp(argv[ii], "active_expire_keys_per_loop")) {
            long long v;
            if (RedisModule_StringToLongLong(argv[ii + 1], &v) == REDISMODULE_ERR) {
                RedisModule_Log(ctx, "warning", "Invalid argument for active_expire_keys_per_loop");
                return REDISMODULE_ERR;
            }
            g_expire_algorithm.keys_per_active_loop = v;
        } else if (!mstrcasecmp(argv[ii], "active_expire_dbs_per_loop")) {
            long long v;
            if (RedisModule_StringToLongLong(argv[ii + 1], &v) == REDISMODULE_ERR) {
                RedisModule_Log(ctx, "warning", "Invalid argument for active_expire_dbs_per_loop");
                return REDISMODULE_ERR;
            }
            g_expire_algorithm.dbs_per_active_loop = v;
        } else if (!mstrcasecmp(argv[ii], "passive_expire_keys_per_loop")) {
            long long v;
            if (RedisModule_StringToLongLong(argv[ii + 1], &v) == REDISMODULE_ERR) {
                RedisModule_Log(ctx, "warning", "Invalid argument for passive_expire_keys_per_loop");
                return REDISMODULE_ERR;
            }
            g_expire_algorithm.keys_per_passive_loop = v;
        } else {
            RedisModule_Log(ctx, "warning", "Unrecognized option");
            return REDISMODULE_ERR;
        }
    }

    RedisModuleTypeMethods tm = {
        .version = REDISMODULE_TYPE_METHOD_VERSION,
        .rdb_load = TairHashTypeRdbLoad,
        .rdb_save = TairHashTypeRdbSave,
        .aof_rewrite = TairHashTypeAofRewrite,
        .free = TairHashTypeFree,
        .digest = TairHashTypeDigest,
#if defined(SORT_MODE) || defined(SLAB_MODE)
        .unlink2 = TairHashTypeUnlink2,
        .copy2 = TairHashTypeCopy2,
        .free_effort2 = TairHashTypeEffort2,
        .mem_usage2 = TairHashTypeMemUsage2,
#else
        .mem_usage = TairHashTypeMemUsage,
        .free_effort = TairHashTypeEffort,
#endif
    };

    TairHashType = RedisModule_CreateDataType(ctx, "tairhash-", TAIRHASH_ENCVER, &tm);
    if (TairHashType == NULL)
        return REDISMODULE_ERR;

    if (RedisModule_GetApi("RedisModule_MallocSizeString", (void **)&g_malloc_size_string) != REDISMODULE_OK) {
        g_malloc_size_string = NULL;
    }
    if (RedisModule_GetApi("RedisModule_CachedMicroseconds", (void **)&g_cached_microseconds) != REDISMODULE_OK) {
        g_cached_microseconds = NULL;
    }
    g_zero_str = RedisModule_CreateString(NULL, "0", 1);

    int (*get_context_flags_all)(void) = NULL;
    if (RedisModule_GetApi("RedisModule_GetContextFlagsAll", (void **)&get_context_flags_all) == REDISMODULE_OK && get_context_flags_all) {
        int restoring_flags = REDISMODULE_CTX_FLAGS_REPLICATED | REDISMODULE_CTX_FLAGS_LOADING;
        g_can_identify_restoring = (get_context_flags_all() & restoring_flags) == restoring_flags;
    }

    if (REDISMODULE_ERR == Module_CreateCommands(ctx)) {
        return REDISMODULE_ERR;
    }

#if defined(SORT_MODE) || defined(SLAB_MODE)
    for (int i = 0; i < DB_NUM; i++) {
        g_expire_index[i] = m_zslCreate();
    }

    RedisModule_SubscribeToServerEvent(ctx, RedisModuleEvent_SwapDB, swapDbCallback);
    RedisModule_SubscribeToServerEvent(ctx, RedisModuleEvent_FlushDB, flushDbCallback);
    RedisModule_SubscribeToKeyspaceEvents(ctx, REDISMODULE_NOTIFY_GENERIC, keySpaceNotification);
    RedisModule_RegisterInfoFunc(ctx, infoFunc);
#endif

#if defined(SLAB_MODE) && defined(__AVX2__)
    slab_initShuffleMask();
#endif

    g_expire_algorithm.insert = insert;
    g_expire_algorithm.update = update;
    g_expire_algorithm.delete = delete;
    g_expire_algorithm.deleteAndPropagate = deleteAndPropagate;
    g_expire_algorithm.activeExpire = activeExpire;
    g_expire_algorithm.passiveExpire = passiveExpire;

    if (g_expire_algorithm.enable_active_expire) {
        /* Here we can't directly use the 'ctx' passed by OnLoad, because
         * in some old version redis `CreateTimer` will trigger a crash, see bugfix:
         * https://github.com/redis/redis/commit/096592506ef3f548a4a3484d5829e04749a24a99
         * https://github.com/redis/redis/commit/7b5f4b175b96dca2093dc1898c3df97e3e096526 */
        RedisModuleCtx *ctx2 = RedisModule_GetThreadSafeContext(NULL);
        startExpireTimer(ctx2, NULL);
        RedisModule_FreeThreadSafeContext(ctx2);
    }
    return REDISMODULE_OK;
}
