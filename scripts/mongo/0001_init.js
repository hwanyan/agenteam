// Agent Runtime 平台 —— MongoDB 初始化脚本
//
// 聊天记录（ChatMessage）具有“按团队追加、按时间顺序读取”的典型日志形态，
// 且未来可能演化出工具调用结果、附件、引用等半结构化字段，
// 更适合用文档数据库承载（相较关系型表需要频繁 ALTER TABLE，文档模型天然支持字段演进）。
//
// 用法：mongosh <uri> scripts/mongo/0001_init.js

db = db.getSiblingDB(process.env.AGENTEAM_MONGO_DATABASE || "agenteam");

db.createCollection("chat_messages");

// team_id + created_at 复合索引：支撑“按团队查询、按时间排序”的核心访问模式。
db.chat_messages.createIndex({ team_id: 1, created_at: 1 });
// 业务主键 id 唯一索引，避免重复写入。
db.chat_messages.createIndex({ id: 1 }, { unique: true });

print("agenteam: chat_messages collection & indexes ready");
