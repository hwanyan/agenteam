// Package mongostore 提供聊天记录（ChatMessage）的 MongoDB 存储实现。
//
// 选型理由：聊天记录是按团队持续追加、按时间顺序读取的日志型数据，
// 且未来可能演化出工具调用结果、引用、附件等半结构化字段，文档模型比
// 关系型表更适合这种字段易变、访问模式简单（基本只按 team_id + 时间过滤）的场景。
package mongostore

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	agenteamv1 "github.com/hwanyan/agenteam/pb/gen"
)

const collectionName = "chat_messages"

// Store 是 store.MessageStore 的 MongoDB 实现。
type Store struct {
	client *mongo.Client
	coll   *mongo.Collection
}

// messageDoc 是聊天记录在 MongoDB 中的文档结构。
// reasoning / tool_calls 是后加的可选字段：历史文档里没有它们，读取时按零值处理即可，
// 无需做数据迁移（这正是聊天记录选用文档库的原因）。
type messageDoc struct {
	ID        string        `bson:"id"`
	TeamID    string        `bson:"team_id"`
	AgentID   string        `bson:"agent_id"`
	Role      int32         `bson:"role"`
	Content   string        `bson:"content"`
	CreatedAt int64         `bson:"created_at"`
	Reasoning string        `bson:"reasoning,omitempty"`
	ToolCalls []toolCallDoc `bson:"tool_calls,omitempty"`
}

// toolCallDoc 是一次工具调用展示信息在 MongoDB 中的结构，与 agenteamv1.ToolCallInfo 一一对应。
type toolCallDoc struct {
	ID        string `bson:"id"`
	Name      string `bson:"name"`
	Kind      string `bson:"kind"`
	Arguments string `bson:"arguments,omitempty"`
	Status    string `bson:"status"`
}

func toToolCallDocs(calls []*agenteamv1.ToolCallInfo) []toolCallDoc {
	if len(calls) == 0 {
		return nil
	}
	out := make([]toolCallDoc, 0, len(calls))
	for _, c := range calls {
		out = append(out, toolCallDoc{ID: c.Id, Name: c.Name, Kind: c.Kind, Arguments: c.Arguments, Status: c.Status})
	}
	return out
}

func fromToolCallDocs(docs []toolCallDoc) []*agenteamv1.ToolCallInfo {
	if len(docs) == 0 {
		return nil
	}
	out := make([]*agenteamv1.ToolCallInfo, 0, len(docs))
	for _, d := range docs {
		out = append(out, &agenteamv1.ToolCallInfo{Id: d.ID, Name: d.Name, Kind: d.Kind, Arguments: d.Arguments, Status: d.Status})
	}
	return out
}

// New 创建 MongoDB Store：建立连接、做连通性检查，并确保索引存在。
func New(ctx context.Context, uri, database string) (*Store, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("mongostore: connect: %w", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, fmt.Errorf("mongostore: ping: %w", err)
	}

	s := &Store{client: client, coll: client.Database(database).Collection(collectionName)}
	if err := s.ensureIndexes(ctx); err != nil {
		_ = client.Disconnect(ctx)
		return nil, err
	}
	return s, nil
}

func (s *Store) ensureIndexes(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "team_id", Value: 1}, {Key: "created_at", Value: 1}}},
		{Keys: bson.D{{Key: "id", Value: 1}}, Options: options.Index().SetUnique(true)},
	})
	if err != nil {
		return fmt.Errorf("mongostore: ensure indexes: %w", err)
	}
	return nil
}

// Close 断开与 MongoDB 的连接。
func (s *Store) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}

// AppendMessage 追加一条聊天记录。
func (s *Store) AppendMessage(ctx context.Context, msg *agenteamv1.ChatMessage) error {
	doc := messageDoc{
		ID:        msg.Id,
		TeamID:    msg.TeamId,
		AgentID:   msg.AgentId,
		Role:      int32(msg.Role),
		Content:   msg.Content,
		CreatedAt: msg.CreatedAt,
		Reasoning: msg.Reasoning,
		ToolCalls: toToolCallDocs(msg.ToolCalls),
	}
	if _, err := s.coll.InsertOne(ctx, doc); err != nil {
		return fmt.Errorf("mongostore: insert message: %w", err)
	}
	return nil
}

// ListMessages 返回指定团队的历史聊天记录，按时间升序排列。
func (s *Store) ListMessages(ctx context.Context, teamID string) ([]*agenteamv1.ChatMessage, error) {
	cur, err := s.coll.Find(ctx,
		bson.M{"team_id": teamID},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}),
	)
	if err != nil {
		return nil, fmt.Errorf("mongostore: find messages: %w", err)
	}
	defer cur.Close(ctx)

	out := make([]*agenteamv1.ChatMessage, 0)
	for cur.Next(ctx) {
		var doc messageDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("mongostore: decode message: %w", err)
		}
		out = append(out, &agenteamv1.ChatMessage{
			Id:        doc.ID,
			TeamId:    doc.TeamID,
			AgentId:   doc.AgentID,
			Role:      agenteamv1.MessageRole(doc.Role),
			Content:   doc.Content,
			CreatedAt: doc.CreatedAt,
			Reasoning: doc.Reasoning,
			ToolCalls: fromToolCallDocs(doc.ToolCalls),
		})
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("mongostore: iterate messages: %w", err)
	}
	return out, nil
}

// DeleteByTeam 删除指定团队的全部聊天记录。
func (s *Store) DeleteByTeam(ctx context.Context, teamID string) error {
	if _, err := s.coll.DeleteMany(ctx, bson.M{"team_id": teamID}); err != nil {
		return fmt.Errorf("mongostore: delete messages: %w", err)
	}
	return nil
}
