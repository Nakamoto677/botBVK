package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
}

func NewClient(token string) *Client {
	return &Client{Token: token, BaseURL: "https://api.telegram.org", HTTP: &http.Client{Timeout: 65 * time.Second}}
}

type APIError struct {
	Code        int
	Description string
	RetryAfter  int
	Ambiguous   bool
}

func (e *APIError) Error() string {
	if e.Ambiguous {
		return fmt.Sprintf("telegram %d: %s; outcome unknown", e.Code, e.Description)
	}
	return fmt.Sprintf("telegram %d: %s", e.Code, e.Description)
}
func (c *Client) Call(ctx context.Context, method string, payload any, result any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/bot"+c.Token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &APIError{Description: "transport failure", Ambiguous: true}
	}
	defer resp.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		Code        int             `json:"error_code"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&envelope); err != nil {
		return &APIError{Code: resp.StatusCode, Description: "invalid API response", Ambiguous: true}
	}
	if !envelope.OK {
		return &APIError{Code: envelope.Code, Description: envelope.Description, RetryAfter: envelope.Parameters.RetryAfter, Ambiguous: resp.StatusCode >= 500}
	}
	if result != nil {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}

type Person struct {
	IsBot     bool   `json:"is_bot"`
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type File struct {
	ID   string `json:"file_id"`
	Name string `json:"file_name"`
	Size int64  `json:"file_size"`
	Path string `json:"file_path"`
}
type Message struct {
	ForumTopicCreated  json.RawMessage `json:"forum_topic_created,omitempty"`
	ForumTopicClosed   json.RawMessage `json:"forum_topic_closed,omitempty"`
	ForumTopicReopened json.RawMessage `json:"forum_topic_reopened,omitempty"`
	ForumTopicEdited   json.RawMessage `json:"forum_topic_edited,omitempty"`
	NewChatMembers     []Person        `json:"new_chat_members,omitempty"`
	LeftChatMember     *Person         `json:"left_chat_member,omitempty"`
	EditDate           int64           `json:"edit_date,omitempty"`
	SenderChat         *Chat           `json:"sender_chat,omitempty"`
	ID                 int64           `json:"message_id"`
	ThreadID           int64           `json:"message_thread_id"`
	From               Person          `json:"from"`
	Chat               Chat            `json:"chat"`
	Text               string          `json:"text"`
	Caption            string          `json:"caption"`
	Reply              *Message        `json:"reply_to_message"`
	Document           *File           `json:"document"`
	Photo              []File          `json:"photo"`
	Voice              *File           `json:"voice"`
	Video              *File           `json:"video"`
	Audio              *File           `json:"audio"`
	Sticker            *File           `json:"sticker"`
	MediaGroupID       string          `json:"media_group_id"`
}
type Callback struct {
	ID      string   `json:"id"`
	From    Person   `json:"from"`
	Data    string   `json:"data"`
	Message *Message `json:"message"`
}
type Update struct {
	Raw           json.RawMessage `json:"-"`
	ID            int64           `json:"update_id"`
	Message       *Message        `json:"message"`
	EditedMessage *Message        `json:"edited_message"`
	Callback      *Callback       `json:"callback_query"`
}

// Retain the original update for durable inbox storage, including fields that
// the current adapter does not support yet.
func (u *Update) UnmarshalJSON(b []byte) error {
	type wire Update
	var v wire
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*u = Update(v)
	u.Raw = append(json.RawMessage(nil), b...)
	return nil
}

func (m Message) serviceMessage() bool {
	return len(m.ForumTopicCreated) > 0 || len(m.ForumTopicClosed) > 0 || len(m.ForumTopicReopened) > 0 || len(m.ForumTopicEdited) > 0 || len(m.NewChatMembers) > 0 || m.LeftChatMember != nil
}

type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data,omitempty"`
	URL  string `json:"url,omitempty"`
}
type Markup struct {
	Buttons [][]Button `json:"inline_keyboard"`
}

func (c *Client) Updates(ctx context.Context, offset int64) ([]Update, error) {
	var result []Update
	err := c.Call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 50, "allowed_updates": []string{"message", "edited_message", "callback_query"}}, &result)
	return result, err
}
func (c *Client) Send(ctx context.Context, chat, topic int64, text string, buttons [][]Button) (Message, error) {
	p := map[string]any{"chat_id": chat, "text": text}
	if topic != 0 {
		p["message_thread_id"] = topic
	}
	if buttons != nil {
		p["reply_markup"] = Markup{buttons}
	}
	var result Message
	err := c.Call(ctx, "sendMessage", p, &result)
	return result, err
}
func (c *Client) Answer(ctx context.Context, id, text string) error {
	return c.Call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}
func (c *Client) Topic(ctx context.Context, chat int64, name string) (int64, error) {
	var result struct {
		ID int64 `json:"message_thread_id"`
	}
	err := c.Call(ctx, "createForumTopic", map[string]any{"chat_id": chat, "name": name}, &result)
	return result.ID, err
}
func (c *Client) CloseTopic(ctx context.Context, chat, topic int64) error {
	return topicLifecycleError(c.Call(ctx, "closeForumTopic", map[string]any{"chat_id": chat, "message_thread_id": topic}, nil))
}
func (c *Client) GetFile(ctx context.Context, id string) (File, error) {
	var f File
	err := c.Call(ctx, "getFile", map[string]any{"file_id": id}, &f)
	return f, err
}
func (c *Client) Download(ctx context.Context, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.BaseURL, "/")+"/file/bot"+c.Token+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download transport failure")
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("download status %d", resp.StatusCode)
	}
	return resp.Body, nil
}
func (c *Client) SendCSV(ctx context.Context, chat, topic int64, text string) error {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	_ = w.WriteField("chat_id", fmt.Sprint(chat))
	if topic != 0 {
		_ = w.WriteField("message_thread_id", fmt.Sprint(topic))
	}
	f, err := w.CreateFormFile("document", "tickets.csv")
	if err != nil {
		return err
	}
	if _, err = io.WriteString(f, text); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.BaseURL, "/")+"/bot"+c.Token+"/sendDocument", &b)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &APIError{Description: "transport failure", Ambiguous: true}
	}
	defer resp.Body.Close()
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("telegram: %s", result.Description)
	}
	return nil
}
func (c *Client) Check(ctx context.Context, cfg Config) error {
	var bot Person
	if err := c.Call(ctx, "getMe", map[string]any{}, &bot); err != nil {
		return err
	}
	for department, team := range cfg.Departments {
		if !department.Valid() || team.ChatID == 0 || team.QueueTopicID == 0 {
			return fmt.Errorf("invalid team configuration for %s", department)
		}
		var chat struct {
			Forum bool `json:"is_forum"`
		}
		if err := c.Call(ctx, "getChat", map[string]any{"chat_id": team.ChatID}, &chat); err != nil {
			return err
		}
		if !chat.Forum {
			return fmt.Errorf("%s chat is not a forum", department)
		}
		var member struct {
			Status string `json:"status"`
			Manage bool   `json:"can_manage_topics"`
			Delete bool   `json:"can_delete_messages"`
		}
		if err := c.Call(ctx, "getChatMember", map[string]any{"chat_id": team.ChatID, "user_id": bot.ID}, &member); err != nil {
			return err
		}
		if member.Status != "creator" && (member.Status != "administrator" || !member.Manage || !member.Delete) {
			return fmt.Errorf("bot needs topic management and delete permissions in %s", department)
		}
	}
	return nil
}

func (c *Client) ReopenTopic(ctx context.Context, chat, topic int64) error {
	return topicLifecycleError(c.Call(ctx, "reopenForumTopic", map[string]any{"chat_id": chat, "message_thread_id": topic}, nil))
}
func topicLifecycleError(err error) error {
	var api *APIError
	if errors.As(err, &api) && api.Code == 400 && strings.Contains(strings.ToUpper(api.Description), "TOPIC_NOT_MODIFIED") {
		return nil
	}
	return err
}
