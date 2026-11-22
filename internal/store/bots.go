package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	WorkspaceKindUser = "user"
	WorkspaceKindBot  = "bot"
)

// Bot represents a registered Faro resident bot with its own isolated home directory.
type Bot struct {
	ID        string
	Slug      string
	Name      string
	HomePath  string
	Model     string
	CreatedAt int64
	UpdatedAt int64
}

// Conversation represents a persisted chat thread (capable of holding one or more bots).
type Conversation struct {
	ID        string
	Title     string
	CreatedAt int64
	UpdatedAt int64
}

// ConversationSummary is a lightweight row for listing recent single-bot conversations.
type ConversationSummary struct {
	ID               string
	BotSlug          string
	BotName          string
	UpdatedAt        int64
	FirstUserMessage string
}

// MessageRecord represents a single persisted chat or tool message in a conversation.
type MessageRecord struct {
	ID             int64
	ConversationID string
	Seq            int
	Role           string
	BotID          string
	Content        string
	ToolCallsJSON  string
	ToolCallID     string
	ToolName       string
	CreatedAt      int64
}

// CreateBot inserts a new bot record in SQLite.
func (s *Store) CreateBot(ctx context.Context, b Bot) (Bot, error) {
	now := time.Now().Unix()
	if b.CreatedAt == 0 {
		b.CreatedAt = now
	}
	if b.UpdatedAt == 0 {
		b.UpdatedAt = now
	}
	_, err := s.writer.ExecContext(ctx, `
		INSERT INTO bots (id, slug, name, home_path, model, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, b.ID, b.Slug, b.Name, b.HomePath, b.Model, b.CreatedAt, b.UpdatedAt)
	if err != nil {
		return Bot{}, fmt.Errorf("create bot %q: %w", b.Slug, err)
	}
	return b, nil
}

// GetBotBySlug returns the bot matching slug, or nil if not found.
func (s *Store) GetBotBySlug(ctx context.Context, slug string) (*Bot, error) {
	return s.queryBot(ctx, `WHERE slug = ?`, slug)
}

// GetBotByID returns the bot matching id, or nil if not found.
func (s *Store) GetBotByID(ctx context.Context, id string) (*Bot, error) {
	return s.queryBot(ctx, `WHERE id = ?`, id)
}

func (s *Store) queryBot(ctx context.Context, where string, arg any) (*Bot, error) {
	var b Bot
	err := s.reader.QueryRowContext(ctx, `
		SELECT id, slug, name, home_path, model, created_at, updated_at
		FROM bots
		`+where, arg).Scan(&b.ID, &b.Slug, &b.Name, &b.HomePath, &b.Model, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// ListBots returns all registered bots ordered by creation time then slug.
func (s *Store) ListBots(ctx context.Context) ([]Bot, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, slug, name, home_path, model, created_at, updated_at
		FROM bots
		ORDER BY created_at, slug
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Bot
	for rows.Next() {
		var b Bot
		if err := rows.Scan(&b.ID, &b.Slug, &b.Name, &b.HomePath, &b.Model, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CreateConversation creates a new conversation and optionally attaches initial participant bots.
func (s *Store) CreateConversation(ctx context.Context, id, title string, botIDs ...string) (Conversation, error) {
	now := time.Now().Unix()
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return Conversation{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO conversations (id, title, created_at, updated_at)
		VALUES (?, ?, ?, ?)
	`, id, title, now, now); err != nil {
		return Conversation{}, fmt.Errorf("create conversation: %w", err)
	}
	for _, bid := range botIDs {
		if bid == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO conversation_participants (conversation_id, bot_id, joined_at)
			VALUES (?, ?, ?)
		`, id, bid, now); err != nil {
			return Conversation{}, fmt.Errorf("add conversation participant: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Conversation{}, err
	}
	return Conversation{
		ID:        id,
		Title:     title,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// GetConversation returns a conversation by ID, or nil if not found.
func (s *Store) GetConversation(ctx context.Context, id string) (*Conversation, error) {
	var c Conversation
	err := s.reader.QueryRowContext(ctx, `
		SELECT id, title, created_at, updated_at
		FROM conversations
		WHERE id = ?
	`, id).Scan(&c.ID, &c.Title, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ConversationHasBot reports whether botID is registered as a participant in conversationID.
func (s *Store) ConversationHasBot(ctx context.Context, conversationID, botID string) (bool, error) {
	var n int
	err := s.reader.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM conversation_participants
		WHERE conversation_id = ? AND bot_id = ?
	`, conversationID, botID).Scan(&n)
	return n > 0, err
}

// ConversationParticipants returns the bot IDs attached to conversationID.
func (s *Store) ConversationParticipants(ctx context.Context, conversationID string) ([]string, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT bot_id FROM conversation_participants
		WHERE conversation_id = ?
		ORDER BY joined_at, bot_id
	`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListConversationsForBot returns conversations that include botID, newest updated first.
func (s *Store) ListConversationsForBot(ctx context.Context, botID string) ([]Conversation, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT c.id, c.title, c.created_at, c.updated_at
		FROM conversations c
		JOIN conversation_participants p ON p.conversation_id = c.id
		WHERE p.bot_id = ?
		ORDER BY c.updated_at DESC, c.id DESC
	`, botID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Conversation
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.Title, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AppendMessages atomically appends one or more messages to conversationID in sequence order.
func (s *Store) AppendMessages(ctx context.Context, conversationID string, msgs []MessageRecord) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var nextSeq int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE conversation_id = ?
	`, conversationID).Scan(&nextSeq); err != nil {
		return err
	}

	now := time.Now().Unix()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO messages (
			conversation_id, seq, role, bot_id, content,
			tool_calls_json, tool_call_id, tool_name, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, m := range msgs {
		ts := m.CreatedAt
		if ts == 0 {
			ts = now
		}
		if _, err := stmt.ExecContext(
			ctx, conversationID, nextSeq, m.Role, m.BotID, m.Content,
			m.ToolCallsJSON, m.ToolCallID, m.ToolName, ts,
		); err != nil {
			return err
		}
		nextSeq++
	}

	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at = ? WHERE id = ?`, now, conversationID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListMessages returns all persisted messages for conversationID in chronological seq order.
func (s *Store) ListMessages(ctx context.Context, conversationID string) ([]MessageRecord, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, conversation_id, seq, role, bot_id, content, tool_calls_json, tool_call_id, tool_name, created_at
		FROM messages
		WHERE conversation_id = ?
		ORDER BY seq ASC, id ASC
	`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MessageRecord
	for rows.Next() {
		var m MessageRecord
		if err := rows.Scan(
			&m.ID, &m.ConversationID, &m.Seq, &m.Role, &m.BotID, &m.Content,
			&m.ToolCallsJSON, &m.ToolCallID, &m.ToolName, &m.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MaxMessageSeq returns the highest persisted message seq for conversationID, or 0 if empty.
func (s *Store) MaxMessageSeq(ctx context.Context, conversationID string) (int, error) {
	var maxSeq int
	err := s.reader.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(seq), 0) FROM messages WHERE conversation_id = ?
	`, conversationID).Scan(&maxSeq)
	return maxSeq, err
}

// ListConversationsWithBot returns recent single-bot conversations newest first, along with each
// conversation's bot slug/name and first user message in a single query.
func (s *Store) ListConversationsWithBot(ctx context.Context, limit int) ([]ConversationSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT
			c.id,
			b.slug,
			b.name,
			c.updated_at,
			COALESCE((
				SELECT m.content
				FROM messages m
				WHERE m.conversation_id = c.id AND m.role = 'user'
				ORDER BY m.seq ASC, m.id ASC
				LIMIT 1
			), '') AS first_user_message
		FROM conversations c
		JOIN conversation_participants p ON p.conversation_id = c.id
		JOIN bots b ON b.id = p.bot_id
		WHERE (SELECT COUNT(*) FROM conversation_participants p2 WHERE p2.conversation_id = c.id) = 1
		ORDER BY c.updated_at DESC, c.id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConversationSummary
	for rows.Next() {
		var item ConversationSummary
		if err := rows.Scan(&item.ID, &item.BotSlug, &item.BotName, &item.UpdatedAt, &item.FirstUserMessage); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
