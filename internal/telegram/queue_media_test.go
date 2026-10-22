package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bot-bvk/internal/domain"
)

type recordedCall struct {
	method string
	body   map[string]any
}

func queueMediaServer(t *testing.T, response func(method string, body map[string]any) string) (*Client, *[]recordedCall) {
	t.Helper()
	calls := []recordedCall{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		calls = append(calls, recordedCall{method: method, body: body})
		_, _ = w.Write([]byte(response(method, body)))
	}))
	t.Cleanup(server.Close)
	client := NewClient("test")
	client.BaseURL = server.URL
	return client, &calls
}

func okMediaResponse(string, map[string]any) string {
	return `{"ok":true,"result":{"message_id":99}}`
}

func photoTicket() (domain.Ticket, domain.Message) {
	ticket := domain.Ticket{ID: 7, ClientID: 1, CompanyID: 10, Department: domain.IT, Status: domain.New, Description: "Не открывается 1С", CreatedAt: time.Date(2026, 10, 20, 14, 9, 0, 0, time.UTC)}
	photo := domain.Message{ID: 8, TicketID: 7, AuthorID: 1, Direction: "inbound", MediaType: "photo", FileID: "photo-file", Text: "Скриншот ошибки", TelegramChatID: 1, TelegramMessageID: 5}
	return ticket, photo
}

func queueWorker(client *Client, store *workerStore) *Worker {
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 2}, domain.OneC: {ChatID: -100, QueueTopicID: 2}}})
	worker.interval = 0
	return worker
}

// The new-ticket message in the queue used to be text-only, so the photo the
// client attached in the form stayed invisible until claim. The queue card now
// itself carries that photo, with the usual description as its caption.
func TestQueueCardCarriesClientPhotoAndDescription(t *testing.T) {
	client, calls := queueMediaServer(t, okMediaResponse)
	ticket, photo := photoTicket()
	store := &workerStore{ticket: ticket, current: ticket, messages: []domain.Message{photo}}
	worker := queueWorker(client, store)
	op := domain.Operation{ID: 5, Kind: "refresh_card", TicketID: ticket.ID}
	if err := worker.Handle(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	var card *recordedCall
	for i := range *calls {
		if (*calls)[i].method == "sendPhoto" {
			card = &(*calls)[i]
		}
	}
	if card == nil {
		t.Fatalf("queue card was not sent as a photo: %+v", *calls)
	}
	if card.body["photo"] != "photo-file" || card.body["parse_mode"] != "HTML" || card.body["message_thread_id"] != float64(2) {
		t.Fatalf("photo card payload = %+v", card.body)
	}
	caption, _ := card.body["caption"].(string)
	if !strings.Contains(caption, "<b>Заявка #7") || !strings.Contains(caption, "Не открывается 1С") {
		t.Fatalf("caption lost the card or description: %q", caption)
	}
	if strings.Contains(caption, "Дополнений от клиента") {
		t.Fatalf("the embedded photo was counted as a supplement: %q", caption)
	}
	buttons, _ := json.Marshal(card.body["reply_markup"])
	if !strings.Contains(string(buttons), "claim:7") {
		t.Fatalf("photo card lost its claim button: %s", buttons)
	}
	if store.current.QueueMessageID != 99 || store.current.QueueMediaType != "photo" {
		t.Fatalf("queue card binding not saved: %+v", store.current)
	}

	// Replaying the same operation must resume from the stored card instead of
	// posting another photo into the queue.
	if err := worker.Handle(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	photos, edits := 0, 0
	for _, call := range *calls {
		switch call.method {
		case "sendPhoto":
			photos++
		case "editMessageCaption":
			edits++
			body, _ := call.body["caption"].(string)
			if !strings.Contains(body, "Не открывается 1С") || call.body["message_id"] != float64(99) {
				t.Fatalf("caption refresh = %+v", call.body)
			}
		}
	}
	if photos != 1 || edits != 1 {
		t.Fatalf("retry duplicated or skipped the card: photos=%d edits=%d", photos, edits)
	}
}

func TestQueueCardFallsBackToTextWhenPhotoIsRejected(t *testing.T) {
	client, calls := queueMediaServer(t, func(method string, _ map[string]any) string {
		if method == "sendPhoto" {
			return `{"ok":false,"error_code":400,"description":"Bad Request: wrong file identifier/HTTP URL specified"}`
		}
		return okMediaResponse(method, nil)
	})
	ticket, photo := photoTicket()
	store := &workerStore{ticket: ticket, current: ticket, messages: []domain.Message{photo}}
	worker := queueWorker(client, store)
	if err := worker.Handle(context.Background(), domain.Operation{ID: 6, Kind: "refresh_card", TicketID: ticket.ID}); err != nil {
		t.Fatal(err)
	}
	var textCard map[string]any
	for _, call := range *calls {
		if call.method == "sendMessage" {
			textCard = call.body
		}
	}
	if textCard == nil {
		t.Fatalf("no text fallback after a rejected photo: %+v", *calls)
	}
	text, _ := textCard["text"].(string)
	if !strings.Contains(text, "Не открывается 1С") || !strings.Contains(text, "<b>Заявка #7") {
		t.Fatalf("text fallback lost the card: %q", text)
	}
	// The fallback card does not carry the photo, so it must still tell the
	// engineer that the client attached something.
	if !strings.Contains(text, "Дополнений от клиента: 1") {
		t.Fatalf("text fallback hid the attachment: %q", text)
	}
	if store.current.QueueMessageID != 99 || store.current.QueueMediaType != "" {
		t.Fatalf("fallback bound the wrong card type: %+v", store.current)
	}
}

// A photo added after the text card was already in the queue still has to show
// up immediately. Bot API can replace a bot message with a media message.
func TestQueueCardUpgradesTextCardWhenPhotoArrivesLater(t *testing.T) {
	client, calls := queueMediaServer(t, okMediaResponse)
	ticket, photo := photoTicket()
	ticket.QueueMessageID = 77
	store := &workerStore{ticket: ticket, current: ticket, messages: []domain.Message{photo}}
	worker := queueWorker(client, store)
	if err := worker.Handle(context.Background(), domain.Operation{ID: 7, Kind: "refresh_card", TicketID: ticket.ID}); err != nil {
		t.Fatal(err)
	}
	var media *recordedCall
	for i := range *calls {
		if (*calls)[i].method == "editMessageMedia" {
			media = &(*calls)[i]
		}
	}
	if media == nil {
		t.Fatalf("text card was not upgraded to media: %+v", *calls)
	}
	input, _ := media.body["media"].(map[string]any)
	if media.body["message_id"] != float64(77) || input["type"] != "photo" || input["media"] != "photo-file" {
		t.Fatalf("upgrade payload = %+v", media.body)
	}
	caption, _ := input["caption"].(string)
	if !strings.Contains(caption, "Не открывается 1С") {
		t.Fatalf("upgrade caption lost the description: %q", caption)
	}
	if store.current.QueueMessageID != 77 || store.current.QueueMediaType != "photo" {
		t.Fatalf("upgrade not remembered: %+v", store.current)
	}
}

func TestQueueCardEditsCaptionAndRetireNoteAfterTransfer(t *testing.T) {
	client, calls := queueMediaServer(t, okMediaResponse)
	ticket, photo := photoTicket()
	ticket.Status = domain.InProgress
	ticket.QueueMessageID = 77
	ticket.QueueMediaType = "photo"
	store := &workerStore{ticket: ticket, current: ticket, messages: []domain.Message{photo}}
	worker := queueWorker(client, store)
	if err := worker.Handle(context.Background(), domain.Operation{ID: 8, Kind: "refresh_card", TicketID: ticket.ID}); err != nil {
		t.Fatal(err)
	}
	captions := 0
	for _, call := range *calls {
		if call.method != "editMessageCaption" {
			continue
		}
		captions++
		caption, _ := call.body["caption"].(string)
		if !strings.Contains(caption, "Не открывается 1С") {
			t.Fatalf("caption refresh lost the description: %q", caption)
		}
	}
	if captions != 1 {
		t.Fatalf("media card was not refreshed through its caption: %+v", *calls)
	}

	*calls = nil
	transferred := ticket
	transferred.Department = domain.OneC
	transferred.Status = domain.New
	transferred.QueueMessageID = 0
	transferred.QueueMediaType = ""
	transferred.TopicID = 0
	transferred.TopicChatID = 0
	store.ticket, store.current = transferred, transferred
	payload, _ := json.Marshal(map[string]any{
		"chat_id": -100, "message_id": 77, "queue_media_type": "photo",
		"from_department": "it", "to_department": "1c",
	})
	if err := worker.Handle(context.Background(), domain.Operation{ID: 9, Kind: "retire_queue_card", TicketID: ticket.ID, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0].method != "editMessageCaption" {
		t.Fatalf("retired media card was not edited as a caption: %+v", *calls)
	}
	caption, _ := (*calls)[0].body["caption"].(string)
	// The photo card caption is built for the *new* department after transfer;
	// it must keep its description and gain the transfer note.
	if !strings.Contains(caption, "Не открывается 1С") || !strings.Contains(caption, "Передано в отдел: 1С") {
		t.Fatalf("retire note lost content: %q", caption)
	}
	if markup, ok := (*calls)[0].body["reply_markup"].(map[string]any); ok && len(markup) != 0 {
		t.Fatalf("retired card kept its keyboard: %+v", (*calls)[0].body)
	}
}

func TestMediaCaptionFitsTheCaptionLimit(t *testing.T) {
	ticket, photo := photoTicket()
	ticket.Description = strings.Repeat("Очень длинное описание проблемы. ", 200)
	store := &workerStore{ticket: ticket, current: ticket, messages: []domain.Message{photo}}
	worker := queueWorker(NewClient("test"), store)
	caption, err := worker.cardCaption(context.Background(), ticket, photo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := visibleTextLength(caption); got > captionLimit {
		t.Fatalf("caption is %d units, limit is %d", got, captionLimit)
	}
	if !strings.Contains(caption, "…") {
		t.Fatal("long description was not marked as shortened")
	}
	if !strings.Contains(caption, "<b>Заявка #7") || !strings.Contains(caption, "<b>Описание</b>\n") {
		t.Fatalf("caption structure lost: %q", caption)
	}
}

func TestVoiceAttachmentKeepsTheTextQueueCard(t *testing.T) {
	client, calls := queueMediaServer(t, okMediaResponse)
	ticket, _ := photoTicket()
	store := &workerStore{ticket: ticket, current: ticket, messages: []domain.Message{{ID: 8, TicketID: 7, Direction: "inbound", MediaType: "voice", FileID: "voice-file"}}}
	worker := queueWorker(client, store)
	if err := worker.Handle(context.Background(), domain.Operation{ID: 10, Kind: "refresh_card", TicketID: ticket.ID}); err != nil {
		t.Fatal(err)
	}
	for _, call := range *calls {
		if call.method == "sendVoice" || call.method == "sendPhoto" {
			t.Fatalf("unsupported attachment became the card: %+v", call)
		}
	}
	if store.current.QueueMediaType != "" {
		t.Fatalf("voice was remembered as a card media type: %+v", store.current)
	}
}

func TestQueueCardMediaTypes(t *testing.T) {
	for mediaType, method := range map[string]string{"photo": "sendPhoto", "video": "sendVideo", "document": "sendDocument"} {
		if got := queueCardMediaMethod(mediaType); got != method {
			t.Fatalf("%s maps to %q, want %q", mediaType, got, method)
		}
	}
	for _, mediaType := range []string{"voice", "audio", "sticker", ""} {
		if got := queueCardMediaMethod(mediaType); got != "" {
			t.Fatalf("%s unexpectedly maps to %q", mediaType, got)
		}
	}
}
