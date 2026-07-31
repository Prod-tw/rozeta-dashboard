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

type rozetaMeetingsPage struct {
	Data  []rozetaMeeting `json:"data"`
	Links struct {
		Next string `json:"next"`
	} `json:"links"`
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
	if result == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("decode Rozeta response: %w", err)
	}
	return nil
}

func (a *app) fetchRozetaMeetings(ctx context.Context, token string) ([]roomMeetingView, error) {
	base, err := url.Parse(a.rozetaURL("/"))
	if err != nil {
		return nil, err
	}
	nextURL := a.rozetaURL("/api/v1/meetings?page=1")
	meetings := make([]roomMeetingView, 0)
	visited := make(map[string]struct{})

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
		for _, meeting := range page.Data {
			meetings = append(meetings, meeting.view())
		}
		nextURL = strings.TrimSpace(page.Links.Next)
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
	return a.doRozeta(req, nil)
}

func (a *app) resumeRozetaMeeting(ctx context.Context, token, meetingID string) error {
	requestURL := a.rozetaURL("/api/v1/meetings/" + url.PathEscape(meetingID) + "/resume")
	req, err := a.newRozetaRequest(ctx, http.MethodPost, requestURL, token, nil)
	if err != nil {
		return err
	}
	var meeting rozetaMeeting
	return a.doRozeta(req, &meeting)
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
