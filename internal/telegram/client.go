package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"sort"
	"strings"
	"time"

	"bot-bvk/internal/metrics"
)

type Client struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
	// Username is filled by Check from getMe. It is used only to recognise an
	// explicit @mention in a company group; an empty value disables hints.
	Username string
	// Metrics collects per-method call latency. It is shared with the workers so a
	// single reading covers the whole process. Nil is allowed and simply skips
	// measurement, which keeps tests free of wiring.
	Metrics *metrics.Metrics
	// retryDelay is the first pause between startup verification attempts; each
	// retry doubles it. Tests shrink it to keep the suite fast.
	retryDelay time.Duration
}

func NewClient(token string) *Client {
	return &Client{Token: token, BaseURL: "https://api.telegram.org", HTTP: &http.Client{Timeout: 65 * time.Second}, Metrics: metrics.New(), retryDelay: time.Second}
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

// publishesNewMessage reports whether a method adds a message to the chat's
// stream. Telegram's per-chat limit ("about one message per second, no more than
// twenty per minute to the same group") counts new messages only, so editing a
// card, creating a forum topic, closing or deleting one must not be charged the
// same spacing. documentMethod is the multipart upload path for files.
func publishesNewMessage(method string) bool {
	switch method {
	case "sendMessage", "sendDocument", "sendPhoto", "sendVideo", "sendAudio", "sendVoice", "sendAnimation", "sendMediaGroup":
		return true
	}
	return false
}

func (c *Client) Call(ctx context.Context, method string, payload any, result any) error {
	// Measure the real round trip per method. This is what tells apart a slow
	// Telegram from time spent waiting in our own rate limiter.
	started := time.Now()
	defer func() {
		if c.Metrics != nil {
			c.Metrics.ObserveCall(method, time.Since(started))
		}
	}()
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
	IsBot                   bool   `json:"is_bot"`
	ID                      int64  `json:"id"`
	FirstName               string `json:"first_name"`
	LastName                string `json:"last_name"`
	Username                string `json:"username"`
	CanJoinGroups           bool   `json:"can_join_groups"`
	CanReadAllGroupMessages bool   `json:"can_read_all_group_messages"`
}
type Chat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Username string `json:"username"`
	Forum    bool   `json:"is_forum"`
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
	MediaGroupID       string          `json:"media_group_id"`
}
type Callback struct {
	ID      string   `json:"id"`
	From    Person   `json:"from"`
	Data    string   `json:"data"`
	Message *Message `json:"message"`
}

// MembershipUpdate is the my_chat_member service update. The adapter only needs
// the chat: this update arrives the moment the bot is added to a group, even
// when privacy mode hides ordinary messages, so it is the reliable way to learn
// the real id of a company group that was configured wrong.
type MembershipUpdate struct {
	Chat Chat `json:"chat"`
}

type Update struct {
	Raw           json.RawMessage   `json:"-"`
	ID            int64             `json:"update_id"`
	Message       *Message          `json:"message"`
	EditedMessage *Message          `json:"edited_message"`
	Callback      *Callback         `json:"callback_query"`
	MyChatMember  *MembershipUpdate `json:"my_chat_member"`
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

// HasRows reports whether the keyboard has anything to draw. An "inline keyboard" with
// no rows is not a keyboard, and Telegram rejects the request outright.
func (m Markup) HasRows() bool {
	for _, row := range m.Buttons {
		if len(row) > 0 {
			return true
		}
	}
	return false
}

// MarshalJSON keeps the field out when there is nothing to draw. Without this, an
// empty button list serialised to {"inline_keyboard":null}, and Telegram answers
// `field "inline_keyboard" must be of type Array`, failing the whole request. Callers
// legitimately build an empty keyboard to strip the buttons off a finished card.
func (m Markup) MarshalJSON() ([]byte, error) {
	if !m.HasRows() {
		return []byte(`{}`), nil
	}
	type plain Markup
	return json.Marshal(plain(m))
}

func (c *Client) Updates(ctx context.Context, offset int64) ([]Update, error) {
	return c.updatesTimeout(ctx, offset, 50)
}
func (c *Client) updatesTimeout(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	var result []Update
	err := c.Call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": timeout, "allowed_updates": []string{"message", "edited_message", "callback_query"}}, &result)
	return result, err
}

// Send delivers text in Telegram-sized chunks. parseMode is optional: engineer
// cards are HTML, client text must stay unparsed because it can contain
// unescaped markup characters.
func (c *Client) Send(ctx context.Context, chat, topic int64, text string, buttons [][]Button, parseMode ...string) (Message, error) {
	var result Message
	parts := splitText(text, messageLimit)
	for i, part := range parts {
		p := map[string]any{"chat_id": chat, "text": part}
		if topic != 0 {
			p["message_thread_id"] = topic
		}
		if len(parseMode) > 0 && parseMode[0] != "" {
			p["parse_mode"] = parseMode[0]
		}
		if buttons != nil && i == len(parts)-1 {
			p["reply_markup"] = Markup{buttons}
		}
		if err := c.Call(ctx, "sendMessage", p, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

// replyKeyboard renders a label matrix as a persistent Telegram reply keyboard.
// The client menu and the staff menu share this shape, so it is built once here
// instead of being assembled by hand at each call site.
func replyKeyboard(keyboard [][]string) map[string]any {
	rows := [][]map[string]string{}
	for _, row := range keyboard {
		items := []map[string]string{}
		for _, label := range row {
			items = append(items, map[string]string{"text": label})
		}
		rows = append(rows, items)
	}
	return map[string]any{"keyboard": rows, "resize_keyboard": true, "is_persistent": true}
}

// HideMessageKeyboard drops the inline buttons of an already delivered message.
func (c *Client) HideMessageKeyboard(ctx context.Context, chat, messageID int64) error {
	return c.Call(ctx, "editMessageReplyMarkup", map[string]any{"chat_id": chat, "message_id": messageID, "reply_markup": Markup{Buttons: [][]Button{}}}, nil)
}

func (c *Client) SendKeyboard(ctx context.Context, chat int64, text string, keyboard [][]string) error {
	return c.Call(ctx, "sendMessage", map[string]any{"chat_id": chat, "text": text, "reply_markup": replyKeyboard(keyboard)}, nil)
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
		Code        int    `json:"error_code"`
		Description string `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.OK {
		return &APIError{Code: result.Code, Description: result.Description, RetryAfter: result.Parameters.RetryAfter}
	}
	return nil
}

// ClientGroupProblem is one company chat that failed startup verification.
// Company chats are optional, so a failure there must not stop the whole bot:
// the caller disables that chat and keeps serving private clients and
// department groups, which carry every other ticket.
type ClientGroupProblem struct {
	ChatID    int64
	CompanyID int64
	Err       error
	// Hint is the operator-facing explanation of what to fix. It is empty when
	// the error already names the defect.
	Hint string
	// Transient marks an answer that can change without a configuration edit:
	// a transport failure, a 5xx or a 429. Such a chat stays enabled, because
	// polling will pick it up as soon as Telegram answers again.
	Transient bool
}

func (p ClientGroupProblem) Error() string {
	if p.Hint == "" {
		return fmt.Sprintf("client group %d: %v", p.ChatID, p.Err)
	}
	return fmt.Sprintf("client group %d: %v (%s)", p.ChatID, p.Err, p.Hint)
}

// Check verifies the whole Telegram setup and reports the first problem. It is
// what -check runs: a release verification must not pass with a half working
// company chat, so here every ClientGroupProblem is fatal.
func (c *Client) Check(ctx context.Context, cfg Config) error {
	problems, err := c.CheckRuntime(ctx, cfg)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return problems[0]
	}
	return nil
}

// CheckRuntime verifies departments strictly and returns company chat problems
// instead of failing the run. Departments stay fatal because every ticket lives
// there; a company chat that Telegram does not answer for is a configuration
// problem the operator can fix without stopping private support.
func (c *Client) CheckRuntime(ctx context.Context, cfg Config) ([]ClientGroupProblem, error) {
	var bot Person
	if err := c.verify(ctx, "getMe", map[string]any{}, &bot); err != nil {
		return nil, err
	}
	c.Username = bot.Username
	for department, team := range cfg.Departments {
		if !department.Valid() || team.ChatID == 0 || team.QueueTopicID == 0 {
			return nil, fmt.Errorf("invalid team configuration for %s", department)
		}
		var chat struct {
			Forum bool `json:"is_forum"`
		}
		if err := c.verify(ctx, "getChat", map[string]any{"chat_id": team.ChatID}, &chat); err != nil {
			return nil, fmt.Errorf("department %s: %w", department, err)
		}
		if !chat.Forum {
			return nil, fmt.Errorf("%s chat is not a forum", department)
		}
		var member struct {
			Status string `json:"status"`
			Manage bool   `json:"can_manage_topics"`
			Delete bool   `json:"can_delete_messages"`
		}
		if err := c.verify(ctx, "getChatMember", map[string]any{"chat_id": team.ChatID, "user_id": bot.ID}, &member); err != nil {
			return nil, fmt.Errorf("department %s: %w", department, err)
		}
		if member.Status != "creator" && (member.Status != "administrator" || !member.Manage || !member.Delete) {
			return nil, fmt.Errorf("bot needs topic management and message cleanup permissions in %s", department)
		}
		// getChat confirms that the group is a forum, but cannot confirm that
		// queue_topic_id still points to a live topic. A brief typing action is
		// accepted only when Telegram can address that thread; it creates no
		// message and prevents a broken queue from reaching the first ticket.
		if err := c.verify(ctx, "sendChatAction", map[string]any{
			"chat_id": team.ChatID, "message_thread_id": team.QueueTopicID, "action": "typing",
		}, nil); err != nil {
			return nil, fmt.Errorf("department %s queue_topic_id=%d in chat_id=%d: %w (check the link to the Queue topic)", department, team.QueueTopicID, team.ChatID, err)
		}
	}
	ids := make([]int64, 0, len(cfg.ClientGroups))
	for id := range cfg.ClientGroups {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var problems []ClientGroupProblem
	for _, id := range ids {
		group := cfg.ClientGroups[id]
		problem := c.checkClientGroup(ctx, bot, id, &group)
		if group.Link != cfg.ClientGroups[id].Link {
			// cfg is a value, but the map is shared with the caller: writing the
			// derived invite link back is what puts it on the engineer cards.
			cfg.ClientGroups[id] = group
		}
		if problem != nil {
			problems = append(problems, *problem)
		}
	}
	return problems, nil
}

// verify runs a read-only startup check with a bounded retry. Telegram is
// normally reachable, so an aborted connection here is a blip on the network
// path, not a configuration defect: failing the whole start over it means a
// working bot stays down until somebody runs it again. Writes are never retried
// here; their idempotency is the outbox's business.
func (c *Client) verify(ctx context.Context, method string, payload any, result any) error {
	const attempts = 3
	for attempt := 1; ; attempt++ {
		err := c.Call(ctx, method, payload, result)
		if err == nil {
			return nil
		}
		if attempt >= attempts || !transientTelegramError(err) {
			return fmt.Errorf("%s: %w", method, err)
		}
		delay := c.retryDelay << (attempt - 1)
		var api *APIError
		if errors.As(err, &api) && api.RetryAfter > 0 {
			delay = time.Duration(api.RetryAfter) * time.Second
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
}

func (c *Client) checkClientGroup(ctx context.Context, bot Person, id int64, group *ClientGroup) *ClientGroupProblem {
	problem := func(err error, hint string) *ClientGroupProblem {
		return &ClientGroupProblem{ChatID: id, CompanyID: group.CompanyID, Err: err, Hint: hint, Transient: transientTelegramError(err)}
	}
	if id >= 0 || group.CompanyID <= 0 {
		return problem(fmt.Errorf("invalid client group configuration"), "chat_id must be the negative id of the company chat")
	}
	var chat struct {
		Type       string `json:"type"`
		Forum      bool   `json:"is_forum"`
		InviteLink string `json:"invite_link"`
	}
	if err := c.verify(ctx, "getChat", map[string]any{"chat_id": id}, &chat); err != nil {
		return problem(err, c.chatHint(id, err))
	}
	if chat.Type != "group" && chat.Type != "supergroup" {
		return problem(fmt.Errorf("not a group chat, Telegram reports type %q", chat.Type), "point chat_id at the company group, not at a user, bot or channel")
	}
	if chat.Forum {
		return problem(fmt.Errorf("the chat is a forum; forum client groups are not supported"), "use a regular group without topics for the company channel")
	}
	var member struct {
		Status string `json:"status"`
	}
	if err := c.verify(ctx, "getChatMember", map[string]any{"chat_id": id, "user_id": bot.ID}, &member); err != nil {
		return problem(err, c.memberHint(id, err))
	}
	if member.Status == "left" || member.Status == "kicked" {
		return problem(fmt.Errorf("bot status is %q", member.Status), fmt.Sprintf("add %s to the company group again", c.botLabel()))
	}
	if !bot.CanReadAllGroupMessages && member.Status != "administrator" && member.Status != "creator" {
		return problem(fmt.Errorf("bot must be an administrator or have privacy mode disabled to see all messages"), fmt.Sprintf("make %s an administrator of the group, or disable its privacy mode in @BotFather", c.botLabel()))
	}
	if group.Link == "" && chat.Type != "supergroup" {
		// A basic group has no t.me/c form, so the card needs an invite link.
		// getChat already carries the primary one when the bot may invite, and
		// reading it changes nothing: exportChatInviteLink is deliberately not
		// called, because it rotates the link the operator may have shared.
		switch {
		case chat.InviteLink != "":
			group.Link = chat.InviteLink
		default:
			// The link is presentation only, so a missing one must not cost the
			// channel: intake, tickets and answers work without it.
			slog.Warn("basic group has no invite link visible to the bot; engineer cards will not open the chat", "chat_id", id)
		}
	}
	return nil
}

// chatHint explains the answers that are impossible to act on as raw text.
// Telegram answers "chat not found" both for a chat the bot was never added to
// and for an id that simply does not exist, so the operator needs the three
// realistic fixes named explicitly.
func (c *Client) chatHint(id int64, err error) string {
	var api *APIError
	if !errors.As(err, &api) {
		return ""
	}
	if !strings.Contains(strings.ToLower(api.Description), "chat not found") {
		return ""
	}
	return fmt.Sprintf("Telegram does not know this chat for %s: the bot is not a member, or the id is wrong. A basic group has no -100 prefix; a group upgraded to a supergroup gets a new id. Add the bot, write any message in the group and read the real id from the \"unrecognised group chat\" warning, or derive it from a t.me/c/<internal>/<message> link as -100<internal>", c.botLabel())
}

func (c *Client) memberHint(id int64, err error) string {
	var api *APIError
	if !errors.As(err, &api) {
		return ""
	}
	description := strings.ToLower(api.Description)
	if strings.Contains(description, "user not found") || strings.Contains(description, "member") {
		return fmt.Sprintf("add %s to the company group %d", c.botLabel(), id)
	}
	return ""
}

func (c *Client) botLabel() string {
	if c.Username == "" {
		return "this token"
	}
	return "@" + c.Username
}

// transientTelegramError keeps a temporary Telegram answer from disabling a
// healthy company chat: only a definite 4xx means the configuration is wrong.
func transientTelegramError(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	return api.Ambiguous || api.Code == 0 || api.Code == 429 || api.Code >= 500
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
