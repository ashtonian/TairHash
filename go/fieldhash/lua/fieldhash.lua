#!lua name=fieldhash

--[[
TairHash (SCAN mode) semantics on core Valkey types.

Each logical key K is stored as two keys sharing the hash tag {K}:
  KEYS[1] "{K}"      hash  field -> struct.pack('<dd', version, expire_at_ms) .. value
  KEYS[2] "{K}:exp"  zset  field -> expire_at_ms, only for fields that have a TTL

expire_at_ms == 0 means no TTL. A field is expired once now_ms >= expire_at_ms,
the same test the module's fieldExpireIfNeeded applies. Expired fields are deleted
when a command touches them, every write reclaims up to PASSIVE_LIMIT due fields
from the index head (the module's passiveExpire), and fh_sweep reclaims the rest.

Numbers are Lua doubles, so versions and counters are exact up to 2^53; the Go
client rejects arguments beyond that and fh_incrby reports overflow past it.
]]

local HEADER = 16
local PASSIVE_LIMIT = 3
local MAX_SAFE_INT = 9007199254740991
local UNPACK_CHUNK = 1000

local ERR_STALE = 'ERR update version is stale'
local ERR_OVERFLOW = 'ERR increment or decrement would overflow'
local ERR_NOT_INTEGER = 'ERR value is not an integer'

local function now_ms()
  local t = server.call('TIME')
  return tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
end

-- delete_fields removes fields from both keys, chunked to stay under Lua's
-- argument-count limit for unpack.
local function delete_fields(d, x, fields)
  for i = 1, #fields, UNPACK_CHUNK do
    local last = math.min(i + UNPACK_CHUNK - 1, #fields)
    server.call('HDEL', d, unpack(fields, i, last))
    server.call('ZREM', x, unpack(fields, i, last))
  end
end

local function passive_expire(d, x, now)
  local due = server.call('ZRANGE', x, '-inf', now, 'BYSCORE', 'LIMIT', 0, PASSIVE_LIMIT)
  if #due > 0 then
    delete_fields(d, x, due)
  end
end

-- lookup returns the live record of field f with its version and deadline, or nil
-- after deleting it if it has expired. now may be nil; the clock is read only when
-- the field has a TTL.
local function lookup(d, x, f, now)
  local rec = server.call('HGET', d, f)
  if not rec then
    return nil
  end
  local ver, exp = struct.unpack('<dd', rec)
  if exp ~= 0 then
    now = now or now_ms()
    if now >= exp then
      server.call('HDEL', d, f)
      server.call('ZREM', x, f)
      return nil
    end
  end
  return rec, ver, exp
end

-- resolve_expiry maps an expiry mode to the new deadline. Mode C clears the TTL,
-- K keeps it, R is relative milliseconds, A absolute Unix milliseconds. Zero
-- relative or absolute times mean "expire now", which the module encodes as 1.
local function resolve_expiry(mode, value, now, current)
  if mode == 'K' then
    return current
  elseif mode == 'C' then
    return 0
  end
  local ms = tonumber(value)
  if ms <= 0 then
    return 1
  elseif mode == 'R' then
    return now + ms
  end
  return ms
end

local function version_ok(mode, want, current)
  if mode == 'VER' then
    return want == 0 or want == current
  elseif mode == 'GT' then
    return want > current
  end
  return true
end

local function next_version(mode, want, current)
  if mode == 'ABS' or mode == 'GT' then
    return want
  end
  return current + 1
end

local function reindex(x, f, old_exp, new_exp)
  if new_exp == 0 then
    if old_exp ~= 0 then
      server.call('ZREM', x, f)
    end
  elseif new_exp ~= old_exp then
    server.call('ZADD', x, new_exp, f)
  end
end

-- fh_set field value exp_mode exp_value cond ver_mode version
-- Returns 1 created, 0 updated, -1 when NX/XX blocked the write.
local function fh_set(keys, args)
  local d, x = keys[1], keys[2]
  local f, value = args[1], args[2]
  local exp_mode, exp_value, cond, ver_mode = args[3], args[4], args[5], args[6]
  local want = tonumber(args[7])

  local now = now_ms()
  passive_expire(d, x, now)

  local rec, cur_ver, cur_exp = lookup(d, x, f, now)
  if rec then
    if cond == 'NX' then
      return -1
    end
    if not version_ok(ver_mode, want, cur_ver) then
      return server.error_reply(ERR_STALE)
    end
  else
    if cond == 'XX' then
      return -1
    end
    cur_ver, cur_exp = 0, 0
  end

  local new_exp = resolve_expiry(exp_mode, exp_value, now, cur_exp)
  server.call('HSET', d, f, struct.pack('<dd', next_version(ver_mode, want, cur_ver), new_exp) .. value)
  reindex(x, f, cur_exp, new_exp)
  if rec then
    return 0
  end
  return 1
end

-- fh_get field
local function fh_get(keys, args)
  local rec = lookup(keys[1], keys[2], args[1], nil)
  if not rec then
    return false
  end
  return string.sub(rec, HEADER + 1)
end

-- fh_getwithver field -> {value, version} or nil
local function fh_getwithver(keys, args)
  local rec, ver = lookup(keys[1], keys[2], args[1], nil)
  if not rec then
    return false
  end
  return { string.sub(rec, HEADER + 1), ver }
end

-- fh_getall -> flat field/value list of live fields
local function fh_getall(keys, args)
  local d, x = keys[1], keys[2]
  local flat = server.call('HGETALL', d)
  local out, dead = {}, {}
  local now
  for i = 1, #flat, 2 do
    local rec = flat[i + 1]
    local _, exp = struct.unpack('<dd', rec)
    local live = true
    if exp ~= 0 then
      now = now or now_ms()
      live = now < exp
    end
    if live then
      out[#out + 1] = flat[i]
      out[#out + 1] = string.sub(rec, HEADER + 1)
    else
      dead[#dead + 1] = flat[i]
    end
  end
  if #dead > 0 then
    delete_fields(d, x, dead)
  end
  return out
end

-- fh_del field [field ...] -> number of live fields removed
local function fh_del(keys, args)
  local d, x = keys[1], keys[2]
  local live, now = 0, nil
  for i = 1, #args, UNPACK_CHUNK do
    local last = math.min(i + UNPACK_CHUNK - 1, #args)
    local recs = server.call('HMGET', d, unpack(args, i, last))
    for j = 1, last - i + 1 do
      local rec = recs[j]
      if rec then
        local _, exp = struct.unpack('<dd', rec)
        if exp == 0 then
          live = live + 1
        else
          now = now or now_ms()
          if now < exp then
            live = live + 1
          end
        end
      end
    end
  end
  delete_fields(d, x, args)
  return live
end

-- fh_incrby field delta exp_mode exp_value ver_mode version min_set min max_set max
local function fh_incrby(keys, args)
  local d, x = keys[1], keys[2]
  local f, delta = args[1], tonumber(args[2])
  local exp_mode, exp_value, ver_mode = args[3], args[4], args[5]
  local want = tonumber(args[6])
  local has_min, min = args[7] == '1', tonumber(args[8])
  local has_max, max = args[9] == '1', tonumber(args[10])

  local now = now_ms()
  passive_expire(d, x, now)

  local rec, cur_ver, cur_exp = lookup(d, x, f, now)
  local current = 0
  if rec then
    local s = string.sub(rec, HEADER + 1)
    if not string.match(s, '^%-?%d+$') then
      return server.error_reply(ERR_NOT_INTEGER)
    end
    current = tonumber(s)
    if math.abs(current) > MAX_SAFE_INT then
      return server.error_reply(ERR_NOT_INTEGER)
    end
    if not version_ok(ver_mode, want, cur_ver) then
      return server.error_reply(ERR_STALE)
    end
  else
    cur_ver, cur_exp = 0, 0
  end

  local result = current + delta
  if math.abs(result) > MAX_SAFE_INT or (has_max and result > max) or (has_min and result < min) then
    return server.error_reply(ERR_OVERFLOW)
  end

  local new_exp = resolve_expiry(exp_mode, exp_value, now, cur_exp)
  server.call('HSET', d, f, struct.pack('<dd', next_version(ver_mode, want, cur_ver), new_exp) .. string.format('%d', result))
  reindex(x, f, cur_exp, new_exp)
  return result
end

-- fh_pttl field -> -2 missing key, -3 missing field, -1 no TTL, else remaining ms
local function fh_pttl(keys, args)
  local d, x, f = keys[1], keys[2], args[1]
  local rec = server.call('HGET', d, f)
  if not rec then
    if server.call('EXISTS', d) == 0 then
      return -2
    end
    return -3
  end
  local _, exp = struct.unpack('<dd', rec)
  if exp == 0 then
    return -1
  end
  local now = now_ms()
  if now >= exp then
    server.call('HDEL', d, f)
    server.call('ZREM', x, f)
    return -3
  end
  return exp - now
end

-- fh_sweep limit -> number of expired fields reclaimed from this key
local function fh_sweep(keys, args)
  local d, x = keys[1], keys[2]
  local due = server.call('ZRANGE', x, '-inf', now_ms(), 'BYSCORE', 'LIMIT', 0, tonumber(args[1]))
  if #due == 0 then
    return 0
  end
  delete_fields(d, x, due)
  return #due
end

server.register_function('fh_set', fh_set)
server.register_function('fh_get', fh_get)
server.register_function('fh_getwithver', fh_getwithver)
server.register_function('fh_getall', fh_getall)
server.register_function('fh_del', fh_del)
server.register_function('fh_incrby', fh_incrby)
server.register_function('fh_pttl', fh_pttl)
server.register_function('fh_sweep', fh_sweep)
