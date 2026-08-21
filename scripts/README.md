# 数据库脚本与本地开发环境

Agent Runtime 平台采用按数据形态拆分的多存储方案，而非单一数据库：

| 存储 | 承载数据 | 选型理由 |
| --- | --- | --- |
| **PostgreSQL** | `teams`、`agents` | 强关系型配置数据：team 与其主 agent 互相引用，创建团队时需要“团队 + 主 Agent”原子写入；后续如需按团队查询/管理多个 Agent，也天然适合关系型的外键与索引。 |
| **Redis** | Agent 运行态热缓存（`agenteam:runtime:agent:{id}`） | 纯粹按 id 读写整块配置快照，无需任何范围查询/join，是典型 KV 场景；同时使“已加载的 Agent 运行态”在多实例部署下可共享，不再是单进程内存态。 |
| **MongoDB** | `chat_messages`（聊天记录） | 按团队追加、按时间顺序读取的日志型数据，且未来可能演化出工具调用结果、引用、附件等半结构化字段；文档模型对字段演进更友好，也更适合这种持续增长的时间序列日志。 |

## 目录结构

```
scripts/
├── docker-compose.yml   # 本地一键拉起 postgres/redis/mongo（可选，仅用于开发）
├── postgres/
│   ├── 0001_init.sql    # 建表 + 索引 + 外键
│   └── 0002_drop.sql    # 回滚脚本
└── mongo/
    └── 0001_init.js     # 建集合 + 索引
```

## 快速开始（Docker）

```bash
docker compose -f scripts/docker-compose.yml up -d

psql "postgres://agenteam:agenteam@localhost:5432/agenteam" -f scripts/postgres/0001_init.sql
mongosh "mongodb://localhost:27017" scripts/mongo/0001_init.js
# redis 无需初始化脚本，首次使用会自动建 key
```

## 快速开始（本机已安装数据库，不使用 Docker）

```bash
# Postgres（示例：本机已有可用实例）
createdb agenteam
psql -d agenteam -f scripts/postgres/0001_init.sql

# Redis
redis-server /opt/homebrew/etc/redis.conf --daemonize yes   # 或 brew services start redis

# MongoDB（brew 需先 `brew tap mongodb/brew` 并信任该 tap；
# 也可直接从 https://www.mongodb.com/try/download/community 下载二进制运行 mongod）
mongod --dbpath <your-data-dir> &
mongosh "mongodb://localhost:27017" scripts/mongo/0001_init.js
```

## 后端连接配置

见项目根 README 的环境变量说明：`AGENTEAM_POSTGRES_DSN` / `AGENTEAM_REDIS_DSN` / `AGENTEAM_MONGO_URI` 等。
