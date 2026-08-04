package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type rozetaAPIError struct {
	StatusCode int
	Message    string
}

func (e *rozetaAPIError) Error() string {
	return e.Message
}

type rozetaProtocolError struct{ err error }

func (e *rozetaProtocolError) Error() string { return e.err.Error() }
func (e *rozetaProtocolError) Unwrap() error { return e.err }

type rozetaRejectedError struct{ message string }

func (e *rozetaRejectedError) Error() string { return e.message }

type rozetaMeetingsPage struct {
	Data  json.RawMessage `json:"data"`
	Links json.RawMessage `json:"links"`
}

type rozetaMeetingLinks struct {
	Next json.RawMessage `json:"next"`
}

type rozetaMeeting struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	StartedAt *int64 `json:"started_at"`
	PausedAt  *int64 `json:"paused_at"`
	UpdatedAt *int64 `json:"updated_at"`
	Languages struct {
		Source string `json:"source"`
		Target string `json:"target"`
	} `json:"languages"`
}

type rozetaSuccessResponse struct {
	Success *bool  `json:"success"`
	Message string `json:"message"`
}

func (a *app) rozetaURL(path string) string {
	baseURL := strings.TrimRight(strings.TrimSpace(a.rozetaBaseURL), "/")
	if baseURL == "" {
		baseURL = "https://rozeta.app"
	}
	return baseURL + path
}

func (a *app) newRozetaRequest(ctx context.Context, method, requestURL, token string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return nil, err
	}
	// Rozeta's command endpoint is documented and verified with Bearer auth, while
	// older meeting deployments accepted the same JWT as a cookie. Sending both keeps
	// server-side compatibility without exposing either credential to the browser.
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Cookie", "auth_token="+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (a *app) doRozeta(req *http.Request, result any) error {
	client := a.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = resp.Status
		}
		return &rozetaAPIError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("Rozeta API returned %s: %s", resp.Status, message)}
	}
	if result == nil {
		return nil
	}
	if resp.StatusCode == http.StatusNoContent {
		return protocolErrorf("Rozeta returned no content for a response with required data")
	}
	decoder := json.NewDecoder(resp.Body)
	// Responses previously accepted the first decodable value and ignored trailing
	// garbage, making a malformed acknowledgement look successful. The complete
	// response must now be one valid JSON value before any action is confirmed.
	if err := decoder.Decode(result); err != nil {
		return &rozetaProtocolError{err: fmt.Errorf("decode Rozeta response: %w", err)}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return protocolErrorf("Rozeta response contained multiple JSON values")
		}
		return &rozetaProtocolError{err: fmt.Errorf("decode trailing Rozeta response: %w", err)}
	}
	return nil
}

func (a *app) fetchRozetaMeetings(ctx context.Context, token, status string) ([]roomMeetingView, error) {
	base, err := url.Parse(a.rozetaURL("/"))
	if err != nil {
		return nil, err
	}
	query := url.Values{"page": {"1"}}
	if status != "" {
		query.Set("status", status)
	}
	nextURL := a.rozetaURL("/api/v1/meetings?" + query.Encode())
	meetings := make([]roomMeetingView, 0)
	visited := make(map[string]struct{})
	meetingIDs := make(map[string]struct{})

	for nextURL != "" {
		next, err := url.Parse(nextURL)
		if err != nil {
			return nil, fmt.Errorf("invalid Rozeta pagination URL: %w", err)
		}
		next = base.ResolveReference(next)
		// Rozeta currently emits same-host HTTP pagination links behind its HTTPS
		// reverse proxy. Following them previously failed the origin check; following
		// them unchanged would expose the Bearer token, so only this exact downgrade is
		// upgraded back to the configured HTTPS scheme.
		if strings.EqualFold(next.Host, base.Host) && base.Scheme == "https" && next.Scheme == "http" {
			next.Scheme = base.Scheme
		}
		// Pagination links carry credentials on the next request. Restricting them to
		// the configured Rozeta origin prevents a compromised response leaking tokens.
		if !strings.EqualFold(next.Scheme, base.Scheme) || !strings.EqualFold(next.Host, base.Host) {
			return nil, fmt.Errorf("Rozeta pagination changed origin from %q to %q", base.String(), next.String())
		}
		if status != "" {
			statusValues := next.Query()["status"]
			if len(statusValues) != 1 || statusValues[0] != status {
				return nil, protocolErrorf("Rozeta pagination dropped or changed status=%s filter", status)
			}
		}
		if _, repeated := visited[next.String()]; repeated {
			return nil, fmt.Errorf("Rozeta pagination repeated %q", next.String())
		}
		if len(visited) >= 100 {
			return nil, errors.New("Rozeta pagination exceeded 100 pages")
		}
		visited[next.String()] = struct{}{}

		req, err := a.newRozetaRequest(ctx, http.MethodGet, next.String(), token, nil)
		if err != nil {
			return nil, err
		}
		var page rozetaMeetingsPage
		if err := a.doRozeta(req, &page); err != nil {
			return nil, err
		}
		pageMeetings, nextPage, err := decodeRozetaMeetingsPage(page)
		if err != nil {
			return nil, err
		}
		for _, meeting := range pageMeetings {
			if err := validateRozetaMeeting(meeting, ""); err != nil {
				return nil, err
			}
			// WHY: the active-set invariant depends on the server honoring its filter.
			// Previously list results were trusted regardless of status; filtered reads now
			// reject any non-matching item instead of pausing or converging from corrupt data.
			if status != "" && meeting.Status != status {
				return nil, protocolErrorf("Rozeta status=%s response contained meeting %q with status %q", status, meeting.ID, meeting.Status)
			}
			if _, duplicate := meetingIDs[meeting.ID]; duplicate {
				return nil, protocolErrorf("Rozeta meetings response repeated meeting %q", meeting.ID)
			}
			meetingIDs[meeting.ID] = struct{}{}
			meetings = append(meetings, meeting.view())
		}
		nextURL = nextPage
	}
	return meetings, nil
}

func (a *app) fetchRozetaMeeting(ctx context.Context, token, meetingID string) (roomMeetingView, error) {
	requestURL := a.rozetaURL("/api/v1/meetings/" + url.PathEscape(meetingID))
	req, err := a.newRozetaRequest(ctx, http.MethodGet, requestURL, token, nil)
	if err != nil {
		return roomMeetingView{}, err
	}
	var meeting rozetaMeeting
	if err := a.doRozeta(req, &meeting); err != nil {
		return roomMeetingView{}, err
	}
	if err := validateRozetaMeeting(meeting, meetingID); err != nil {
		return roomMeetingView{}, err
	}
	return meeting.view(), nil
}

func (a *app) sendRozetaCommand(ctx context.Context, token, action, targetID string) error {
	payload, err := json.Marshal(struct {
		Action   string `json:"action"`
		TargetID string `json:"target_id"`
	}{Action: action, TargetID: targetID})
	if err != nil {
		return err
	}
	req, err := a.newRozetaRequest(ctx, http.MethodPost, a.rozetaURL("/api/v1/commands"), token, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	return a.readRozetaCommandResponse(req)
}

func (a *app) sendRozetaEmbedCommand(ctx context.Context, token, targetID string) error {
	payload, err := json.Marshal(struct {
		Action   string `json:"action"`
		TargetID string `json:"target_id"`
		ClientID string `json:"client_id"`
		Payload  struct {
			FontSize  int    `json:"font_size"`
			Layout    string `json:"layout"`
			Multiline bool   `json:"multiline"`
		} `json:"payload"`
	}{
		Action:   "goto_meeting_embed",
		TargetID: targetID,
		ClientID: "obs",
		Payload: struct {
			FontSize  int    `json:"font_size"`
			Layout    string `json:"layout"`
			Multiline bool   `json:"multiline"`
		}{FontSize: 2, Layout: "split-horizontal", Multiline: false},
	})
	if err != nil {
		return err
	}
	req, err := a.newRozetaRequest(ctx, http.MethodPost, a.rozetaURL("/api/v1/commands"), token, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	return a.readRozetaCommandResponse(req)
}

func (a *app) readRozetaCommandResponse(req *http.Request) error {
	var response rozetaSuccessResponse
	if err := a.doRozeta(req, &response); err != nil {
		return err
	}
	if response.Success == nil {
		return protocolErrorf("Rozeta command response omitted success")
	}
	if !*response.Success {
		message := strings.TrimSpace(response.Message)
		if message == "" {
			message = "command was not accepted"
		}
		return &rozetaRejectedError{message: fmt.Sprintf("Rozeta command failed: %s", message)}
	}
	return nil
}

func (a *app) resumeRozetaMeeting(ctx context.Context, token, meetingID string) error {
	requestURL := a.rozetaURL("/api/v1/meetings/" + url.PathEscape(meetingID) + "/resume")
	req, err := a.newRozetaRequest(ctx, http.MethodPost, requestURL, token, nil)
	if err != nil {
		return err
	}
	var meeting rozetaMeeting
	if err := a.doRozeta(req, &meeting); err != nil {
		return err
	}
	return validateRozetaMeeting(meeting, meetingID)
}

func decodeRozetaMeetingsPage(page rozetaMeetingsPage) ([]rozetaMeeting, string, error) {
	if len(page.Data) == 0 || string(page.Data) == "null" {
		return nil, "", protocolErrorf("Rozeta meetings response omitted data array")
	}
	var meetings []rozetaMeeting
	if err := json.Unmarshal(page.Data, &meetings); err != nil {
		return nil, "", &rozetaProtocolError{err: fmt.Errorf("decode Rozeta meetings data: %w", err)}
	}
	if len(page.Links) == 0 || string(page.Links) == "null" {
		return nil, "", protocolErrorf("Rozeta meetings response omitted links object")
	}
	var links rozetaMeetingLinks
	if err := json.Unmarshal(page.Links, &links); err != nil {
		return nil, "", &rozetaProtocolError{err: fmt.Errorf("decode Rozeta meetings links: %w", err)}
	}
	if len(links.Next) == 0 {
		return nil, "", protocolErrorf("Rozeta meetings response omitted links.next")
	}
	if string(links.Next) == "null" {
		return meetings, "", nil
	}
	var next string
	if err := json.Unmarshal(links.Next, &next); err != nil {
		return nil, "", &rozetaProtocolError{err: fmt.Errorf("decode Rozeta meetings links.next: %w", err)}
	}
	return meetings, strings.TrimSpace(next), nil
}

func validateRozetaMeeting(meeting rozetaMeeting, requestedID string) error {
	// Zero-value meeting fields previously flowed into reconciliation and could let
	// another meeting authorize Resume. Require identity and a documented status so
	// malformed observations now fail before they can change controller state.
	if strings.TrimSpace(meeting.ID) == "" {
		return protocolErrorf("Rozeta meeting response omitted id")
	}
	if requestedID != "" && meeting.ID != requestedID {
		return protocolErrorf("Rozeta returned meeting %q for requested meeting %q", meeting.ID, requestedID)
	}
	switch meeting.Status {
	case "ready", "paused", "in_progress", "completed":
		return nil
	default:
		return protocolErrorf("Rozeta returned unknown meeting status %q", meeting.Status)
	}
}

func protocolErrorf(format string, arguments ...any) error {
	return &rozetaProtocolError{err: fmt.Errorf(format, arguments...)}
}

func (meeting rozetaMeeting) view() roomMeetingView {
	return roomMeetingView{
		ID:        meeting.ID,
		Title:     meeting.Title,
		Status:    meeting.Status,
		Source:    meeting.Languages.Source,
		Target:    meeting.Languages.Target,
		StartedAt: unixTime(meeting.StartedAt),
		PausedAt:  unixTime(meeting.PausedAt),
		UpdatedAt: unixTime(meeting.UpdatedAt),
	}
}

func unixTime(value *int64) time.Time {
	if value == nil {
		return time.Time{}
	}
	return time.Unix(*value, 0).UTC()
}
