![](https://img.shields.io/badge/license-Apache--2.0-green)
![](https://img.shields.io/badge/PRs-welcome-green)
[![Build](https://github.com/alibaba/TairHash/actions/workflows/cmake.yml/badge.svg)](https://github.com/alibaba/TairHash/actions/workflows/cmake.yml)
[![CI](https://github.com/alibaba/TairHash/actions/workflows/ci.yml/badge.svg)](https://github.com/alibaba/TairHash/actions/workflows/ci.yml)
[![Docker](https://github.com/alibaba/TairHash/actions/workflows/docker-image.yml/badge.svg)](https://github.com/alibaba/TairHash/actions/workflows/docker-image.yml)
[![codecov](https://codecov.io/gh/chenyang8094/TairHash/branch/develop/graph/badge.svg?token=9A8QP7MTN3)](https://codecov.io/gh/chenyang8094/TairHash)  


<div align=center>
<img src="imgs/tairhash_logo.jpg" width="500"/>
</div>

## 简介  [英文说明](README.md)
     TairHash是基于redis module开发的一种hash数据结构，和redis原生的hash数据结构相比，TairHash不但和原生hash一样具有丰富的数据接口和高性能，还可以为field设置过期时间和版本，这极大的提高了hash数据结构的灵活性，在很多场景下可以大大的简化业务开发。TairHash提供active expire机制，即使field在过期后没有被访问也可以被主动删除，释放内存。


### 主要的特性如下：

- 支持redis hash的所有命令语义
- field支持单独设置expire和version
- 针对field支持高效的active expire和passivity expire，其中active expire支持SCAN_MODE、SORT_MODE和SLAB_MODE模式。
- 支持field过期删除事件通知（基于pubsub）

## 内存

每个field是一次内存分配，包含名称、值、版本和过期时间，并直接挂在哈希表的链上。一个包含30个field（名称和值均为8字节）的key总共约占1.9KB（`bench/profile.sh`，Redis 7.0，jemalloc），而每个field使用一个dict entry、一个值结构体和两个字符串对象时约为5.2KB。只有当key的某个field设置了过期时间时才会分配过期索引。`MEMORY USAGE` 返回分配器为该key实际持有的内存。

## 主动过期

### SCAN_MODE（扫描模式）：
- 不对TairHash进行全局排序（可以节省内存）
- 每个TairHash内部依然会使用一个排序索引对fields进行排序（加速每个key内部的field查找）
- 内置定时器会周期使用SCAN命令找到包含过期field的TairHash，然后检查TairHash内部的排序索引，进行field的淘汰
- 排序中所有的key和field都是指针引用，无内存拷贝，无内存膨胀问题

**支持的版本**: redis >= 5.0, valkey >= 7.2  
**优点**：可以运行在低版本的redis中（redis >= 5.0 ）      
**缺点**：过期淘汰效率较低（相对SORT模式而言，特别是redis中含有过多的干扰key时）  

**使用方式**：以默认选项执行cmake，并重新编译
### SORT_MODE（排序模式）：

- 使用两级排序索引，第一级对tairhash主key进行排序，第二级针对每个tairhash内部的field进行排序
- 第一级排序使用第二级排序里最小的ttl进行排序，因此主key是按照ttl全局有序的
- 内置定时器会周期扫描第一级索引，找出一部分已经过期的key，然后分别再检查这些key的二级索引，进行field的淘汰，也就是active expire
- 排序中所有的key和field都是指针引用，无内存拷贝，无内存膨胀问题

**支持的版本**: redis >= 7.0, valkey >= 7.2  
**优点**：过期淘汰效率比较高      
**缺点**：更多的内存消耗  

**使用方式**：cmake的时候加上`-DSORT_MODE=yes`选项，并重新编译
### SLAB_MODE（slab模式）：

- SLAB模式是一种节省内存，缓存友好，高性能的过期算法
- 和SORT模式一样，也会依赖key的全局排序索引进行过期key的快速查找。和SORT模式不同的是，SLAB不会对key内部的field进行排序索引，相反他们是无序的，这样可以节省索引内存开销‘
- SLAB过期算法使用SIMD指令（当硬件支持时）来加速对过期field的查找

**支持的版本**: redis >= 7.0, valkey >= 7.2  

**优点**：高效淘汰，低内存消耗，快速访问，为过期算法带来新思路  

**缺点**：更多的内存消耗  

**使用方式**：cmake的时候加上`-DSLAB_MODE=yes`选项，并重新编译

## 主动过期
- 每一次读写field，会触发对这个field自身的过期淘汰操作  
- 每次写一个field时，TairHash也会检查其它field（可能属于其它的key）是否已经过期（每次最多检查3个），因为field是按照TTL排序的，因此这个检查会很高效 (注意: SLAB_MODE暂时不支持这个功能)

## 事件通知  

tairhash在field发生过期时（由主动或被动过期触发）会发送一个事件通知，通知以pubsub方式发送，channel的格式为：`tairhash@<db>@<key>__:<event>` , 目前只支持expired事件类型，因此
channel为：`tairhash@<db>@<key>__:expired`，消息内容为过期的field。

## 快速开始

```go
127.0.0.1:6379> EXHSET k f v ex 10
(integer) 1
127.0.0.1:6379> EXHGET k f
"v"
127.0.0.1:6379> EXISTS k
(integer) 1
127.0.0.1:6379> debug sleep 10
OK
(10.00s)
127.0.0.1:6379> EXISTS k
(integer) 0
127.0.0.1:6379> EXHGET k f
(nil)
127.0.0.1:6379> EXHSET k f v px 10000
(integer) 1
127.0.0.1:6379> EXHGET k f
"v"
127.0.0.1:6379> EXISTS k
(integer) 1
127.0.0.1:6379> debug sleep 10
OK
(10.00s)
127.0.0.1:6379> EXISTS k
(integer) 0
127.0.0.1:6379> EXHGET k f
(nil)
127.0.0.1:6379> EXHSET k f v  VER 1
(integer) 1
127.0.0.1:6379> EXHSET k f v  VER 1
(integer) 0
127.0.0.1:6379> EXHSET k f v  VER 1
(error) ERR update version is stale
127.0.0.1:6379> EXHSET k f v  ABS 1
(integer) 0
127.0.0.1:6379> EXHSET k f v  ABS 2
(integer) 0
127.0.0.1:6379> EXHVER k f
(integer) 2
```  

## Docker
```
docker run -p 6379:6379 tairmodule/tairhash:latest
```
## 编译及使用

```
mkdir build  
cd build  
cmake ../ && make -j
```
编译成功后会在lib目录下产生tairhash_module.so库文件

```
./redis-server --loadmodule /path/to/tairhash_module.so
./valkey-server --loadmodule /path/to/tairhash_module.so
```  

## 模块参数

在模块路径后以 `name value` 的形式传入参数，例如
`--loadmodule /path/to/tairhash_module.so replication_format compat`。

| 参数 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `enable_active_expire` | 0或1 | `1` | 是否运行主动过期定时器。关闭后field只会在访问时被动过期。 |
| `active_expire_period` | 毫秒 | `1000` | 主动过期的执行间隔。 |
| `active_expire_keys_per_loop` | 整数 | `1000` | 每次主动过期在每个数据库中检查的key数量（SCAN模式下为SCAN的count）。 |
| `active_expire_dbs_per_loop` | 整数 | `16` | 每次主动过期访问的数据库数量。 |
| `passive_expire_keys_per_loop` | 整数 | `3` | 每次被动过期检查最多删除的过期field数量。 |
| `replication_format` | `state` 或 `compat` | `state` | 写入如何传给从节点和AOF：`state` 每次写入发送一条内部命令 `EXHMSETREPL`，`compat` 每个field发送一条 `EXHSET`/`EXHSETNX`，TairHash 1.x也能识别。见[从1.x升级](#从1x升级)。在Redis 7.0+和Valkey上它同时是模块配置 `tairhash.replication_format`，可以通过 `CONFIG SET` 在运行时修改。 |

## 从1.x升级

TairHash 2.0把每次写入复制为一条 `EXHMSETREPL`，TairHash 1.x不认识该命令：2.0主节点的1.x从节点会对每次写入记录错误并丢弃该写入。RDB编码不变。在所有节点都运行2.0之前，请使用 `replication_format compat`：

1. 所有升级后的节点都以 `replication_format compat` 启动（在Redis 7.0+和Valkey上，也可以在配置文件中写 `tairhash.replication_format compat`）。先升级从节点，进行故障转移，再升级原主节点：故障转移期间1.x节点可能会从2.0节点复制，`compat` 可以保证其正常工作。
2. 确认每个节点（包括级联的从节点）的 `MODULE LIST` 中 `tairhash` 的 `ver` 为 `20000`：从节点会原样转发其主节点的复制流，因此任意层级的从节点收到的都是顶层主节点发送的内容。
3. 将所有节点切换为 `state`：在Redis 7.0+和Valkey上执行 `CONFIG SET tairhash.replication_format state`，更早的版本去掉该参数后重启。

降级时，先切换回 `compat`，执行 `BGREWRITEAOF` 使AOF中不再包含 `EXHMSETREPL`，再替换模块。在Redis 7.0+和Valkey上，升级期间设置 `propagation-error-behavior panic-on-replicas` 可以让无法应用复制流的从节点直接停止，而不是静默地产生数据不一致。

`EXHSETNX` 现在与 `EXHSET` 一样，新field的版本从1开始；1.x中为0，导致之后的 `VER` 检查对任意版本都会通过。
编译选项（`cmake -D<选项>=ON ../`）：

| 选项 | 默认值 | 说明 |
|---|---|---|
| `SORT_MODE` / `SLAB_MODE` | `OFF` | 主动过期算法（默认SCAN模式） |
| `TAIRHASH_NATIVE_ARCH` | `OFF` | 针对编译机器的CPU优化（`-march=native`）。仅适用于在运行机器上编译：这样编译的二进制在其他CPU上可能因SIGILL崩溃，例如在CI上构建的Docker镜像。SLAB模式无需开启即可在运行时选择AVX2路径。 |
| `TAIRHASH_LTO` | `OFF` | 链接时优化 |
| `TAIRHASH_OUTPUT_DIR` | `lib/` | 模块库的输出目录 |
| `SANITIZER_MODE` | 未设置 | `address`、`undefined` 或 `thread` |
| `GCOV_MODE` | `OFF` | 覆盖率插桩 |

## 测试方法

`tests/run.sh` 会编译模块，并在Redis或Valkey源码的测试框架中运行 `tests/` 下的TCL测试。CI的每个任务都通过该脚本运行，因此本地运行与CI执行相同的步骤。服务端源码只会在 `.deps/` 下克隆并编译一次；每种模块变体编译到 `build/` 下各自的目录。

```
tests/run.sh                                  # 所有测试，SCAN模式，Redis 7.0
tests/run.sh --mode sort --suite tairhash     # 指定测试与过期模式
tests/run.sh --sanitizer address              # 使用ASan编译模块与Redis
tests/run.sh --coverage                       # 输出模块的行/函数覆盖率
tests/run.sh --server-ref 7.2 -- --baseport 25000   # 其他Redis分支，附加runtest参数
tests/run.sh --server valkey --server-ref 9.1.3     # 使用Valkey
```

依赖：C编译工具链、CMake 3.10+、git、Tcl 8.5+。macOS上 `long double` 为64位，`Exhincrbyfloat*` 测试会失败，Linux上正常。

手动运行：将测试文件中的 `your_path` 替换为 `tairhash_module.so` 所在目录，复制到Redis或Valkey源码的 `tests/unit/type/` 下，运行 `./runtest --single unit/type/<suite>`。

CI在x86-64与arm64上针对Redis 5.0、6.2、7.0、7.2、7.4、8.10以及Valkey 7.2、8.1、9.1运行所有测试；其中Redis 7.0、Redis 8.10和Valkey 9.1覆盖所有过期模式，SCAN和SORT模式还会在AddressSanitizer下运行。

## 客户端

| language | GitHub |
|----------|---|
| Java     |https://github.com/alibaba/alibabacloud-tairjedis-sdk|
| Python   |https://github.com/alibaba/tair-py|
| Go       |https://github.com/alibaba/tair-go|
| .Net     |https://github.com/alibaba/AlibabaCloud.TairSDK|

## API
[参考这里](CMDDOC-CN.md)

## 我们的modules

[TairHash](https://github.com/alibaba/TairHash): 和redis hash类似，但是可以为field设置expire和version，支持高效的主动过期和被动过期   
[TairZset](https://github.com/alibaba/TairZset): 和redis zset类似，但是支持多（最大255）维排序，同时支持incrby语义，非常适合游戏排行榜场景   
[TairString](https://github.com/alibaba/TairString): 和redis string类似，但是支持设置expire和version，并提供CAS/CAD等实用命令，非常适用于分布式锁等场景  


