# test_tikv_client_go

使用 Go TiKV Client (client-go) 直接读取 TiKV 底层 KV 数据，并解析 TiDB 的 Key 编码，反查出 table_id、row_id 等信息。

## 背景

TiDB 是建立在 TiKV 之上的 SQL 层。TiKV 本身是纯 KV 存储，没有表、列、schema 的概念。TiDB 通过自定义的 Key/Value 编码规则，将 SQL 表的行和索引映射到 TiKV 的 KV 对中。

本项目通过 TiKV Go Client (client-go) 直接扫描 TiKV 中的原始 KV 数据，并解码出 TiDB 层面的 table_id、row_id / index_id 等信息，再通过 TiDB SQL 接口反向验证。

## 依赖

```go
require github.com/tikv/client-go/v2 v2.0.7
```

- Go >= 1.21
- 需要能访问 PD (Placement Driver) 节点以及 TiKV 节点的网络

## 编译与运行

```bash
go build -o test_tikv_client_go .
./test_tikv_client_go <PD_ADDR> [OPTIONS]

# 基础示例
./test_tikv_client_go 10.0.12.184:2379

# 指定 table_id 过滤
./test_tikv_client_go 10.0.12.184:2379 --table-id 11875

# 限制扫描条数
./test_tikv_client_go 10.0.12.184:2379 --table-id 11875 --limit 5

# 使用 TransactionClient (推荐，能读到 TiDB 写入的数据)
./test_tikv_client_go 10.0.12.184:2379 --table-id 11875 --limit 5 --txn

# 只扫描行记录 (跳过索引)
./test_tikv_client_go 10.0.12.184:2379 --table-id 11875 --txn --type record

# 只扫描索引
./test_tikv_client_go 10.0.12.184:2379 --table-id 11875 --txn --type index

# 如果连接超时可加 timeout
timeout 10 ./test_tikv_client_go 10.0.12.184:2379
```

### 命令行参数

| 参数 | 说明 | 示例 |
|------|------|------|
| `<PD_ADDR>` | PD 节点地址，支持多个 | `10.0.12.184:2379 10.0.10.43:2379` |
| `--table-id <ID>` | 过滤指定 table_id 的数据 | `--table-id 11875` |
| `--limit <N>` | 限制扫描的 KV 对数量 (默认 20) | `--limit 5` |
| `--txn` | 使用 TransactionClient 而非 RawClient | `--txn` |
| `--type <record\|index>` | 过滤 key 类型：只扫行记录或只扫索引 | `--type record` |

### RawClient vs TransactionClient

TiKV 提供两种客户端模式：

| 模式 | 说明 | 适用场景 |
|------|------|---------|
| **RawClient** | 直接读取 RawKV 层，不走 MVCC | 适用于原生使用 RawKV API 的数据 |
| **TransactionClient** | 通过事务层读取，走 MVCC | **适用于 TiDB 写入的数据（推荐）** |

**关键区别**：

- TiDB 写入的数据是通过 **Transaction API** 存储的，存在于 TiKV 的 MVCC 层
- RawClient 只能访问 RawKV 层，**看不到** TiDB 写入的数据
- TransactionClient 能正确读取 TiDB 数据，并且不会在 key 末尾带 MVCC 版本号（已在内部处理）

**Go 客户端使用方式**：

```go
// RawClient
import "github.com/tikv/client-go/v2/rawkv"
client, err := rawkv.NewClient(ctx, pdAddrs, config.DefaultConfig().Security)
keys, values, err := client.Scan(ctx, startKey, endKey, limit)

// TransactionClient
import "github.com/tikv/client-go/v2/txnkv"
client, err := txnkv.NewClient(pdAddrs)
txn, err := client.Begin()
iter, err := txn.Iter(startKey, endKey)
```

**实测案例**：

```bash
# RawClient 读不到数据
$ ./test_tikv_client_go 10.0.12.184:2379 --table-id 11875 --limit 5
Found 0 key-value pairs

# TransactionClient 正常读取
$ ./test_tikv_client_go 10.0.12.184:2379 --table-id 11875 --limit 5 --txn
Found 5 key-value pairs
```

### 网络注意事项

TiKV Client 连接 PD 后，PD 会返回 TiKV 节点的地址。如果 PD 返回的是内网 IP（如 `10.0.x.x`），而你从外部访问，需要做地址映射。可以用 iptables DNAT：

```bash
sudo iptables -t nat -A OUTPUT -d 10.0.12.184 -j DNAT --to-destination <公网IP1>
sudo iptables -t nat -A OUTPUT -d 10.0.10.43  -j DNAT --to-destination <公网IP2>
```

## TiDB 的 Key 编码详解

### 整体架构

```
TiDB (SQL 层)
  ↓ 编码
TiKV (KV 存储层)
  Key:   bytes
  Value: bytes
```

TiKV 中的每一个 KV 对，在 TiDB 看来就是一行记录或一条索引。

### Key 的逻辑格式

TiDB 有两种 Key 类型：

| 类型 | 格式 | 说明 |
|------|------|------|
| 行记录 (Record) | `t{table_id}_r{row_id}` | 一行数据 |
| 索引 (Index)    | `t{table_id}_i{index_id}{index_value}` | 一条索引项 |

具体字节布局（逻辑层）：

```
行记录 Key (19 bytes):
┌──────┬──────────────────┬──────┬──────────────────┐
│ 't'  │ table_id (8B)    │ '_r' │ row_id (8B)      │
│ 0x74 │ big-endian i64   │ 5f72 │ big-endian i64   │
└──────┴──────────────────┴──────┴──────────────────┘

索引 Key (19+ bytes):
┌──────┬──────────────────┬──────┬──────────────────┬─────────────┐
│ 't'  │ table_id (8B)    │ '_i' │ index_id (8B)    │ index_value │
│ 0x74 │ big-endian i64   │ 5f69 │ big-endian i64   │ ...         │
└──────┴──────────────────┴──────┴──────────────────┴─────────────┘
```

### 整数编码方式 (Comparable Encoding)

table_id、row_id 等 i64 值使用 **符号位翻转 + big-endian** 编码，使得编码后的字节序与数值大小一致（可直接按字节比较排序）：

```
编码: 将 i64 转为 big-endian 8 字节，然后将第一个字节 XOR 0x80
解码: 将第一个字节 XOR 0x80，然后按 big-endian 读取 i64
```

示例：

| 值 | big-endian hex | 编码后 hex |
|----|---------------|-----------|
| 0  | `00 00 00 00 00 00 00 00` | `80 00 00 00 00 00 00 00` |
| 1  | `00 00 00 00 00 00 00 01` | `80 00 00 00 00 00 00 01` |
| 24 | `00 00 00 00 00 00 00 18` | `80 00 00 00 00 00 00 18` |
| -1 | `ff ff ff ff ff ff ff ff` | `7f ff ff ff ff ff ff ff` |

**符号位翻转的作用**：

- 对于无符号数，字节序天然有序（`0x00 < 0x01 < ... < 0xff`）
- 对于有符号数，负数的最高位是 1，会大于正数的最高位 0，导致 `负数 > 正数`
- XOR 0x80 将符号位翻转后：
  - 负数的 `1xxx xxxx` → `0xxx xxxx`（变小）
  - 正数的 `0xxx xxxx` → `1xxx xxxx`（变大）
  - 结果：`负数 < 0 < 正数`，字节序与数值序一致

对应代码 (`decodeI64`)：

```go
func decodeI64(data []byte) int64 {
    buf := make([]byte, 8)
    copy(buf, data[:8])
    buf[0] ^= 0x80 // flip sign bit
    return int64(binary.BigEndian.Uint64(buf))
}
```

### 索引 Key 的编码

索引 Key 在 `table_id + _i + index_id` 之后，还会追加索引列的值。每列值按类型编码：

**列类型编码格式**：

| 类型 | 前缀字节 | 后续编码 | 说明 |
|------|---------|---------|------|
| Int (正数) | `0x03` | 8 字节 i64 (符号位翻转) | 整数类型 |
| Int (负数) | `0x04` | 8 字节 i64 (符号位翻转) | 整数类型 |
| Bytes/String | `0x01` | memcomparable bytes 编码 | 变长字符串 |

**示例：解析 UNIQUE KEY `idx_profileid_tag` (`profile_id`, `tag`)**

```
Table: updatelog_esdoc_tagsinfo
CREATE TABLE `updatelog_esdoc_tagsinfo` (
  `profile_id` bigint NOT NULL,
  `tag` varchar(64) NOT NULL,
  UNIQUE KEY `idx_profileid_tag` (`profile_id`,`tag`)
) PARTITION BY HASH (`profile_id`) PARTITIONS 32
```

索引 Key 格式：`t{table_id}_i{index_id} + {profile_id} + {tag}`

实际 hex：

```
74 80 00 00 00 00 00 2e 63 5f 69 80 00 00 00 00 00 00 01 03 80 00 00 00 00 00 10 80 01 32 30 32 35 30 39 5f 32 ff 30 32 35 31 31 5f 75 70 ff 64 61 74 65 00 00 00 00 fb
│  └─── table_id=11875 ──┘ └tag┘ └─ index_id=1 ────┘ │  └─ profile_id=4224 ──┘ │  └──────── tag="202509_202511_update" ──────────┘
t                                                      0x03 (int)                0x01 (bytes)
```

解码结果：

```
key (tidb): table_id=11875, index_id=1
key (index): int=4224, str="202509_202511_update"
```

### Memcomparable Bytes 编码

TiKV 在逻辑 Key 之上还包了一层 **memcomparable bytes** 编码，保证编码后的字节序仍然等价于原始 Key 的排序。

规则：

- 每 8 字节数据后跟 1 字节标记（marker），共 9 字节一组
- 标记 `0xff`：本组 8 字节全部有效，后面还有更多组
- 标记 `0xff - N`：本组最后 N 字节是填充（`0x00`），只有前 `8 - N` 字节有效，这是最后一组

```
原始字节:     [b0 b1 b2 b3 b4 b5 b6 b7] [b8 b9 bA ...]
              ↓
编码后:       [b0 b1 b2 b3 b4 b5 b6 b7 ff] [b8 b9 bA 00 00 00 00 00 fc]
              ──────── 8字节 ──── marker    ──── 8字节(含填充) ── marker
                                 (全有效)                        (0xff-0xfc=3字节有效+5填充)
```

对应代码 (`decodeMemcomparableBytes`)：

```go
func decodeMemcomparableBytes(encoded []byte) ([]byte, int) {
    var decoded []byte
    pos := 0
    for {
        if pos+9 > len(encoded) {
            break
        }
        group := encoded[pos : pos+8]
        marker := encoded[pos+8]
        pos += 9
        if marker == 0xff {
            decoded = append(decoded, group...)
        } else {
            padCount := int(0xff - marker)
            if padCount <= 8 {
                decoded = append(decoded, group[:8-padCount]...)
            }
            break
        }
    }
    return decoded, pos
}
```

### 索引 Value 的编码

对于 **唯一索引（UNIQUE INDEX）**，TiDB 将 handle (row_id) 存储在索引的 **Value** 中，用于回表查询。

**Value 格式（简化）**：

```
[version_info] + handle (_tidb_rowid) + [restore_data]
```

- **handle**：用于回表，即 `t{table_id}_r{handle}` 的 key
- **restore_data**：存储索引列的原始值（用于 restore 非 binary collation 的字符串）

### Record vs Index：Key 的字节序排序

在 TiKV 中，同一 `table_id` 下，**索引 key 会排在行记录 key 之前**，原因是：

```
'_i' (0x69) < '_r' (0x72)
```

因此在按字节序扫描时，会先遇到索引条目，再遇到行记录。

如果只想扫描行记录，使用 `--type record`：

```bash
$ ./test_tikv_client_go 10.0.12.184:2379 --table-id 11875 --txn --type record
```

此时 scan 的起始 key 为 `t{table_id}_r`，跳过了所有索引。

### MVCC 版本号

在 memcomparable 编码的 Key 之后，TiKV 还会追加 8 字节的 MVCC 版本号（时间戳）。

- **RawClient** scan 时这部分会出现在 key 末尾（需要手动解析）
- **TransactionClient** 在内部已处理 MVCC，返回的 key 不带版本号

### 完整解码流程

```
TiKV 中的原始 Key bytes
  │
  ├─ Step 1: Memcomparable 解码 → 逻辑 Key + 剩余 MVCC 版本号
  │
  ├─ Step 2: 解析逻辑 Key
  │   ├─ key[0] = 't' (0x74)
  │   ├─ key[1..9] → decodeI64 → table_id
  │   ├─ key[9..11] = "_r" 或 "_i"
  │   └─ key[11..19] → decodeI64 → row_id 或 index_id
  │
  └─ Step 3: 通过 table_id 在 TiDB 中反查表名
```

对应代码 (`decodeTiDBKeyDetailed`)：

```go
func decodeTiDBKeyDetailed(rawKey []byte) string {
    key, consumed := decodeMemcomparableBytes(rawKey)
    remaining := len(rawKey) - consumed
    // key[0] == 't', key[1:9] → tableID, key[9:11] → tag, key[11:19] → rowID/indexID
    ...
}
```

## Value 的行格式

TiDB 的 Value 使用自定义行格式（Row Format v2），将一行的所有列值紧凑编码成字节流。

**编码特点**：

- 每列按类型编码（int、string、datetime 等各有格式）
- 字符串/blob 类型的列值使用 memcomparable bytes 编码
- Value 的二进制数据中，ASCII 字符串会被 `0xff` 标记字节打断

`extractASCIIStrings` 函数在提取可读字符串时会跳过夹在 ASCII 字符之间的 `0xff` 字节，从而还原完整的字符串。

## 案例演示

### 1. 扫描索引条目并解析字段

**场景**：扫描 TiDB 表 `updatelog_esdoc_tagsinfo` (table_id=11875) 的索引 `idx_profileid_tag`。

**执行**：

```bash
$ ./test_tikv_client_go 10.0.12.184:2379 --table-id 11875 --limit 3 --txn
```

**预期输出**：

```
=== TiKV Scan Test (TransactionClient) ===
PD endpoints: [10.0.12.184:2379]
Filter: table_id=11875
Limit: 3

start_key (hex): 74 80 00 00 00 00 00 2e 63
end_key   (hex): 74 80 00 00 00 00 00 2e 64

Connecting (TransactionClient)...
Connected!

Scanning 3 keys for [table_id=11875]...
Found 3 key-value pairs:

--- [0] ---
  key (hex):  74 80 00 00 00 00 00 2e 63 5f 69 80 00 00 00 00 00 00 01 ...
  key (tidb): table_id=11875, index_id=1
  key (index): int=4224, str="202509_202511_update"
  val (hex):  08 80 00 02 00 00 00 01 02 ...
  val (len):  43 bytes
  val (parsed): handle_candidates=[{1 2199023255554} ...]
  val (handle): _tidb_rowid=4224 (at offset 1)
  val (strings): 202509_202511_update
```

**TiDB 验证**：

```sql
mysql> SELECT * FROM updatelog_esdoc_tagsinfo WHERE _tidb_rowid = 4224;
+------------+---------------------------------------+---------------------+---------------------+
| profile_id | tag                                   | gmt_create          | gmt_modified        |
+------------+---------------------------------------+---------------------+---------------------+
|   88851681 | es_main, es_exp: update null location | 2025-11-22 02:42:22 | 2025-11-22 02:42:22 |
+------------+---------------------------------------+---------------------+---------------------+
```

### 2. 只扫描行记录 (跳过索引)

使用 `--type record` 只扫描行记录：

```bash
$ ./test_tikv_client_go 10.0.12.184:2379 --table-id 11875 --limit 3 --txn --type record
```

此时 scan 起始 key 为 `t{table_id}_r`，直接跳过所有索引条目。

### 3. 通过 TiDB SQL 反查表名

```sql
SELECT TABLE_SCHEMA, TABLE_NAME, TIDB_TABLE_ID
FROM information_schema.tables
WHERE TIDB_TABLE_ID = 24;
```

### 4. 通过 _tidb_rowid 伪列验证具体行

对于 `NONCLUSTERED` 主键的表，TiDB 使用隐式的 `_tidb_rowid` 作为 row_id：

```sql
SELECT *, _tidb_rowid
FROM mysql.stats_histograms
WHERE _tidb_rowid = 284237;
```

## 与 Rust 版本的对应关系

本项目是 [test_tikv_client](../test_tikv_client/) (Rust 版本) 的 Go 语言复刻。功能完全一致，主要区别在于客户端库的 API：

| 功能 | Rust (tikv-client) | Go (client-go) |
|------|-------------------|----------------|
| RawClient 创建 | `RawClient::new(pd_endpoints)` | `rawkv.NewClient(ctx, pdAddrs, security)` |
| TransactionClient 创建 | `TransactionClient::new(pd_endpoints)` | `txnkv.NewClient(pdAddrs)` |
| 开始事务 | `client.begin_optimistic()` | `client.Begin()` |
| Raw 扫描 | `client.scan(start..end, limit)` | `client.Scan(ctx, start, end, limit)` |
| 事务扫描 | `txn.scan(start..end, limit)` | `txn.Iter(start, end)` + 手动限制条数 |
| 提交事务 | `txn.commit()` | `txn.Commit(ctx)` |
| 异步运行时 | tokio | 不需要（Go 原生并发） |

## 函数索引

| 函数 | 作用 |
|------|------|
| `decodeMemcomparableBytes` | 解码 memcomparable bytes 编码，每 9 字节一组（8 数据 + 1 标记） |
| `decodeTiDBKeyDetailed`   | 详细解码：memcomparable + table_id + tag + row_id/index_id + 索引列 |
| `decodeLogicalKeyDetailed`| 解码逻辑 key（TransactionClient 返回的 key） |
| `decodeIndexColumns`       | 解码索引 key 中的列值（int、string 等类型） |
| `decodeIndexValue`         | 解码索引 value 中的 handle (_tidb_rowid) |
| `decodeI64`                | 解码符号位翻转的 big-endian i64 |
| `encodeI64`                | 编码 i64 为符号位翻转的 big-endian |
| `encodeMemcomparableBytes` | 编码 memcomparable bytes |
| `encodeTablePrefix`        | 编码表前缀 key（用于 RawClient 扫描范围） |
| `extractASCIIStrings`      | 从二进制 value 中提取可读 ASCII 字符串（跳过 0xff 标记） |
| `bytesToHex`               | 字节转 hex 字符串显示 |
| `isIndexKey`               | 判断 key 是否为索引 key (_i) |

## 参考

- [TiDB Key-Value 映射](https://docs.pingcap.com/tidb/stable/tidb-computing#mapping-table-data-to-key-value)
- [TiKV Go Client (GitHub)](https://github.com/tikv/client-go)
- [client-go Wiki](https://github.com/tikv/client-go/wiki)
- [Rust 版本 (test_tikv_client)](../test_tikv_client/)
