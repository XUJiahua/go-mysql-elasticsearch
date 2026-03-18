# 多集群 Active-Standby 方案

## 背景

当前同步进度（binlog position）保存在本地文件 `{data_dir}/master.info`，仅适合单机部署。目标是支持多 K8s 集群部署多活：同时只有一个实例是 Active，其余 Standby，当 Active 所在集群挂掉时 Standby 自动接管继续同步。

需要改造三个层面：**Position 存储分布式化**、**Leader 选举**、**生命周期管理**。

## 1. Position 存储：从本地文件改为分布式存储

当前 `masterInfo` 直接读写本地文件，需要抽象为接口，支持多种后端：

```go
// river/position.go
type PositionStore interface {
    Load() (mysql.Position, error)
    Save(pos mysql.Position) error
    Close() error
}
```

### 后端选型对比

| 后端 | 优点 | 缺点 |
|------|------|------|
| **etcd** | 天然支持租约和 Watch，与 K8s 生态一致 | 额外运维组件 |
| **MySQL**（复用源库） | 无额外依赖 | 源库挂了 position 也读不到（但此时同步本身也停了） |
| **Redis** | 低延迟 | 需考虑持久化策略 |

**推荐 MySQL**，因为不引入额外组件，且源库不可用时同步本身也无意义。

### 存储表结构

```sql
CREATE TABLE sync_position (
    lock_name  VARCHAR(64) PRIMARY KEY,
    bin_name   VARCHAR(255),
    bin_pos    BIGINT,
    leader_id  VARCHAR(255),
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
);
```

### 代码改造点

`river.go:51` 的 `loadMasterInfo(c.DataDir)` 改为根据配置选择实现：

```go
if c.PositionStoreType == "mysql" {
    r.master, err = newMySQLPositionStore(c)
} else {
    r.master, err = newFilePositionStore(c.DataDir) // 原逻辑
}
```

## 2. Leader 选举：确保单 Active

核心问题：多个实例只能有一个在消费 binlog 并写 ES。

### 推荐方案：基于 MySQL `GET_LOCK` 的分布式锁（零额外依赖）

```go
// river/leader.go
type LeaderElection struct {
    db       *sql.DB
    lockName string
    leaderID string      // 当前实例标识（hostname + pod name）
    ctx      context.Context
    cancel   context.CancelFunc
}

func (l *LeaderElection) Campaign(ctx context.Context) error {
    // SELECT GET_LOCK('gme_leader', timeout)
    // 阻塞直到获取锁或 ctx 取消
}

func (l *LeaderElection) Resign() {
    // SELECT RELEASE_LOCK('gme_leader')
}

func (l *LeaderElection) KeepAlive(ctx context.Context) {
    // 维持 MySQL 连接保活，连接断开即自动释放锁
}
```

**为什么能工作：** MySQL `GET_LOCK` 是连接级别的锁。Active 实例持有连接，如果实例挂了或网络断了，MySQL 连接超时后自动释放锁，Standby 实例立刻获取。

### GET_LOCK 机制说明

`GET_LOCK(name, timeout)` 是 MySQL 提供的用户级别（User-Level）命名锁，也叫 Advisory Lock。

**基本用法：**

```sql
-- 获取锁，等待最多10秒，获取成功返回1，超时返回0
SELECT GET_LOCK('my_lock', 10);

-- 释放锁
SELECT RELEASE_LOCK('my_lock');

-- 检查锁是否空闲（不获取），1=空闲，0=被占用
SELECT IS_FREE_LOCK('my_lock');
```

**核心特性：**

- **连接级别绑定** — 锁与 MySQL 连接（session）绑定，不与事务绑定。`COMMIT`/`ROLLBACK` 不会释放锁。连接断开（客户端崩溃、网络中断、超时）时锁自动释放。这正是本方案用它做 Leader 选举的基础：进程挂了 → 连接断 → 锁释放 → Standby 获取。
- **互斥性** — 同一个 `name` 全局唯一，任意时刻只有一个 session 能持有。其他 session 调用 `GET_LOCK` 会阻塞直到超时或锁释放。
- **与行锁/表锁无关** — 不锁任何表或行，纯粹是一个命名信号量，不影响正常的 DML 操作。
- **MySQL 5.7+** — 同一 session 可以同时持有多个命名锁（5.7 之前只能持有一个，获取新锁会隐式释放旧锁）。

**在本方案中的用法：**

Standby 用一个很长的 timeout（如 3600 秒）调用 `GET_LOCK`，持续阻塞等待，无需自己写轮询逻辑。Active 一旦断连，MySQL 自动释放锁，Standby 立刻获取成功。

**版本要求：**

本方案不引入额外的 MySQL 版本要求。`GET_LOCK` 从 MySQL 3.x 起就已存在，本方案只需持有一把锁 `gme_leader`，不依赖 MySQL 5.7+ 的多锁特性。只要满足项目原有的 MySQL 版本要求即可。MariaDB 方面也一样，`GET_LOCK` 行为一致。

**注意事项：**

- **锁名是全局的** — 不区分 database，命名需要加前缀避免冲突（如 `gme_leader`）。
- **不会写 binlog** — `GET_LOCK` 不会被复制到 MySQL 从库，锁状态仅存在于连接的那个 MySQL 实例。
- **连接保活** — 必须维持持有锁的连接不被 MySQL 的 `wait_timeout` 踢掉，需要定期发心跳（如 `SELECT 1`）。

### 备选方案：K8s Lease

使用 `client-go/tools/leaderelection`，好处是原生支持、无额外组件，但引入了对 K8s API 的强依赖。

## 3. 生命周期改造

### 当前流程

```
main → NewRiver → Run → syncLoop
```

### 改造后流程

```
main → NewRiver → Campaign(阻塞等待成为Leader)
                → 获取锁成功 → LoadPosition → Run → syncLoop
                                                   ↓
                              锁丢失/续约失败 ← KeepAlive 监控
                                   ↓
                              Close → Resign → 回到 Campaign
```

### `River.Run()` 改造示意

```go
func (r *River) Run() error {
    for {
        // 1. 竞选 Leader（阻塞）
        if err := r.leader.Campaign(r.ctx); err != nil {
            return err
        }
        log.Infof("became leader: %s", r.leader.leaderID)

        // 2. 从分布式存储加载最新 position
        pos, _ := r.master.Load()

        // 3. 启动同步（用子 context，丢锁时可单独取消）
        syncCtx, syncCancel := context.WithCancel(r.ctx)
        go r.keepAlive(syncCtx, syncCancel) // 监控锁状态

        r.wg.Add(1)
        go r.syncLoop()
        err := r.canal.RunFrom(pos)

        // 4. 同步结束（主动关闭或丢锁），清理后重新竞选
        syncCancel()
        r.canal.Close()
        r.wg.Wait()
        r.leader.Resign()

        if r.ctx.Err() != nil {
            return nil // 主 context 取消 = 真正退出
        }
        // 否则重新竞选
    }
}
```

## 4. 防脑裂（Fencing）

关键风险：旧 Leader 网络分区后恢复，在新 Leader 已经接管后继续往 ES 写数据。

### 防护措施

**Position 写入带 leader_id 校验：**

`Save()` 时校验 `leader_id` 是否仍是自己，不是则立刻停止：

```sql
UPDATE sync_position
SET bin_name=?, bin_pos=?, updated_at=NOW()
WHERE lock_name=? AND leader_id=?
```

`affected_rows == 0` 表示身份已被抢占，立刻 cancel。

**syncLoop 中检查 context：**

当前代码 `sync.go:150` 已经有 `case <-r.ctx.Done(): return`，丢锁时 cancel context 即可让 syncLoop 停止。

**ES 写入的幂等性：**

已有的 at-least-once 语义天然兼容切主场景：新 Leader 从旧 position 重放，ES 的 `index` 操作是幂等的（相同 `_id` 覆盖写）。

## 5. 改造文件清单

| 文件 | 改动 |
|------|------|
| `river/config.go` | 新增 `PositionStoreType`、`LeaderLockName` 等配置项 |
| `river/position.go` | **新增** `PositionStore` 接口 + MySQL 实现 |
| `river/master.go` | 改造为 `filePositionStore`，实现 `PositionStore` 接口 |
| `river/leader.go` | **新增** Leader 选举逻辑 |
| `river/river.go` | `River` 结构体加 `leader` 字段，`Run()` 加竞选循环 |
| `river/sync.go` | 无需大改，context 取消机制已具备 |
| `etc/river.toml` | 新增配置段 |

## 6. 架构图

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        Multi-Cluster Active-Standby                        │
└─────────────────────────────────────────────────────────────────────────────┘

  K8s Cluster A                          K8s Cluster B
 ┌──────────────────────┐              ┌──────────────────────┐
 │                      │              │                      │
 │  ┌────────────────┐  │              │  ┌────────────────┐  │
 │  │  gme instance  │  │              │  │  gme instance  │  │
 │  │   (Active)     │  │              │  │   (Standby)    │  │
 │  │                │  │              │  │                │  │
 │  │ ┌────────────┐ │  │              │  │ ┌────────────┐ │  │
 │  │ │ syncLoop   │ │  │              │  │ │ Campaign() │ │  │
 │  │ │ 消费binlog  │ │  │              │  │ │ 阻塞等待锁  │ │  │
 │  │ │ 写入ES     │ │  │              │  │ │            │ │  │
 │  │ └────────────┘ │  │              │  │ └────────────┘ │  │
 │  │                │  │              │  │                │  │
 │  │ ┌────────────┐ │  │              │  └────────────────┘  │
 │  │ │ keepAlive  │ │  │              │                      │
 │  │ │ 维持锁连接  │ │  │              └──────────┬───────────┘
 │  │ └────────────┘ │  │                         │
 │  └───┬──┬──┬──────┘  │                         │
 │      │  │  │         │                         │
 └──────┼──┼──┼─────────┘                         │
        │  │  │                                   │
        │  │  │    GET_LOCK('gme_leader') 阻塞     │
        │  │  │                                   │
 ───────┼──┼──┼───────────────────────────────────┼──────────
        │  │  │         Shared Infrastructure     │
        │  │  │                                   │
        │  │  │  ┌──────────────────────────────┐ │
        │  │  └─▶│         MySQL (源库)          │◀┘
        │  │     │                              │
        │  │     │  binlog ──────────────────┐  │
        │  │     │                           │  │
        │  │     │  sync_position 表         │  │
        │  │     │  ┌──────────┬──────────┐  │  │
        │  │     │  │ bin_name │ bin_pos  │  │  │
        │  │     │  │ leader_id│updated_at│  │  │
        │  │     │  └──────────┴──────────┘  │  │
        │  │     │                           │  │
        │  │     │  GET_LOCK 分布式锁         │  │
        │  │     │  (连接级别，断连自动释放)    │  │
        │  │     └──────────────────────────────┘ │
        │  │                                      │
        │  │     ┌──────────────────────────────┐  │
        │  └────▶│       Elasticsearch          │  │
        │        │                              │  │
        │        │  index 操作幂等（同 _id 覆写）│  │
        │        └──────────────────────────────┘  │
        │                                          │
 ───────┼──────────────────────────────────────────┘
        │
        ▼


 ════════════════════════════════════════════════════════════
                      Failover 时序
 ════════════════════════════════════════════════════════════

  Cluster A (Active)          MySQL                Cluster B (Standby)
       │                        │                        │
       │──── GET_LOCK ─────────▶│                        │
       │◀─── OK (持有锁) ───────│                        │
       │                        │◀── GET_LOCK (阻塞) ────│
       │                        │                        │
       │   binlog消费 + ES写入   │                        │
       │──── Save position ────▶│                        │
       │                        │                        │
       ╳ Cluster A 宕机          │                        │
       ╳ MySQL连接断开           │                        │
                                │                        │
                                │── 锁自动释放 ──────────▶│
                                │◀─ GET_LOCK OK ─────────│
                                │                        │
                                │◀─ Load position ───────│
                                │                        │
                                │        binlog消费 + ES写入
                                │                        │
```

## 总结

核心思路：**把 `masterInfo` 接口化做分布式存储 + 用 MySQL `GET_LOCK` 做零依赖的 Leader 选举 + Position 写入时校验身份防脑裂**。整体改动集中在 `river/` 包，对 `elastic/` 和 `sync.go` 的事件处理逻辑几乎无侵入。
