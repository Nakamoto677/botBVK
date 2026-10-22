package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
)

func (c *Client) SendLocalFile(ctx context.Context, chat, topic int64, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if name == "" {
		name = "attachment.bin"
	}
	var b bytes.Buffer
	writer := multipart.NewWriter(&b)
	if err = writer.WriteField("chat_id", fmt.Sprint(chat)); err != nil {
		return err
	}
	if topic != 0 {
		if err = writer.WriteField("message_thread_id", fmt.Sprint(topic)); err != nil {
			return err
		}
	}
	part, err := writer.CreateFormFile("document", name)
	if err != nil {
		return err
	}
	if _, err = io.Copy(part, f); err != nil {
		return err
	}
	if err = writer.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.BaseURL, "/")+"/bot"+c.Token+"/sendDocument", &b)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := c.HTTP.Do(req)
	if err != nil {
		return &APIError{Description: "upload transport failure", Ambiguous: true}
	}
	defer response.Body.Close()
	var result struct {
		OK          bool   `json:"ok"`
		Code        int    `json:"error_code"`
		Description string `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return &APIError{Description: "invalid upload response", Ambiguous: true}
	}
	if !result.OK {
		return &APIError{Code: result.Code, Description: result.Description, RetryAfter: result.Parameters.RetryAfter, Ambiguous: response.StatusCode >= 500}
	}
	return nil
}
