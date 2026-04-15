package management

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	liveMonitorMaxWindow       = 24 * time.Hour
	liveMonitorDefaultPageSize = 6
	liveMonitorDefaultRange    = 6 * time.Hour
	liveMonitorRetention       = 7 * 24 * time.Hour
	liveMonitorHistoryStep     = 20
)

type liveMonitorStatus string

const (
	liveMonitorStatusOnline  liveMonitorStatus = "online"
	liveMonitorStatusError   liveMonitorStatus = "error"
	liveMonitorStatusOffline liveMonitorStatus = "offline"
)

type liveMonitorPersistedEvent struct {
	Timestamp       time.Time `json:"timestamp"`
	Kind            string    `json:"kind"`
	RequestID       string    `json:"request_id,omitempty"`
	ClientIP        string    `json:"client_ip,omitempty"`
	RequestedModel  string    `json:"requested_model,omitempty"`
	RequestedType   string    `json:"requested_type,omitempty"`
	ActualModel     string    `json:"actual_model,omitempty"`
	ActualType      string    `json:"actual_type,omitempty"`
	ChannelPrefix   string    `json:"channel_prefix,omitempty"`
	ChannelLabel    string    `json:"channel_label,omitempty"`
	ChannelBaseURL  string    `json:"channel_base_url,omitempty"`
	AuthID          string    `json:"auth_id,omitempty"`
	ResultStatus    string    `json:"result_status,omitempty"`
	StateTransition string    `json:"state_transition,omitempty"`
	FallbackReason  string    `json:"fallback_reason,omitempty"`
	ErrorMessage    string    `json:"error_message,omitempty"`
	HTTPStatus      int       `json:"http_status,omitempty"`
}

type liveMonitorHistoryEntry struct {
	ID            string `json:"id"`
	At            string `json:"at"`
	Kind          string `json:"kind"`
	Result        string `json:"result,omitempty"`
	ChannelPrefix string `json:"channel_prefix,omitempty"`
	ActualType    string `json:"actual_type,omitempty"`
	ActualModel   string `json:"actual_model,omitempty"`
	AuthID        string `json:"auth_id,omitempty"`
	Reason        string `json:"reason,omitempty"`
	ErrorMessage  string `json:"error_message,omitempty"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	RequestID     string `json:"request_id,omitempty"`
}

type liveMonitorActiveRequestItem struct {
	RequestID     string `json:"request_id"`
	Status        string `json:"status"`
	ChannelPrefix string `json:"channel_prefix,omitempty"`
	StartedAt     string `json:"started_at,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
	ErrorSummary  string `json:"error_summary,omitempty"`
	ActualModel   string `json:"actual_model,omitempty"`
	ActualType    string `json:"actual_type,omitempty"`
}

type liveMonitorIssueItem struct {
	At            string `json:"at"`
	ChannelPrefix string `json:"channel_prefix,omitempty"`
	Reason        string `json:"reason,omitempty"`
	ErrorMessage  string `json:"error_message,omitempty"`
}

type liveMonitorCard struct {
	CardID                string                 `json:"card_id"`
	MaskedIP              string                 `json:"masked_ip"`
	RequestedModel        string                 `json:"requested_model"`
	DisplayModel          string                 `json:"display_model"`
	RequestedType         string                 `json:"requested_type,omitempty"`
	ActualType            string                 `json:"actual_type,omitempty"`
	PrimaryChannelPrefix  string                 `json:"primary_channel_prefix,omitempty"`
	ExtraChannelCount     int                    `json:"extra_channel_count"`
	Concurrency           int                    `json:"concurrency"`
	Status                liveMonitorStatus      `json:"status"`
	UpdatedAt             string                 `json:"updated_at"`
	RecentIssues          []liveMonitorIssueItem `json:"recent_issues,omitempty"`
	TypeMismatch          bool                   `json:"type_mismatch"`
	TypeMismatchRequested string                 `json:"type_mismatch_requested,omitempty"`
	TypeMismatchActual    string                 `json:"type_mismatch_actual,omitempty"`
	TypeMismatchAt        string                 `json:"type_mismatch_at,omitempty"`
	RecentResultIcon      string                 `json:"recent_result_icon,omitempty"`
}

type liveMonitorCardPage struct {
	GeneratedAt string            `json:"generated_at"`
	Range       string            `json:"range"`
	Page        int               `json:"page"`
	PageSize    int               `json:"page_size"`
	TotalCards  int               `json:"total_cards"`
	TotalPages  int               `json:"total_pages"`
	Cards       []liveMonitorCard `json:"cards"`
}

type liveMonitorDetail struct {
	CardID         string                         `json:"card_id"`
	FullIP         string                         `json:"full_ip"`
	RequestedModel string                         `json:"requested_model"`
	DisplayModel   string                         `json:"display_model"`
	RequestedType  string                         `json:"requested_type,omitempty"`
	ActualType     string                         `json:"actual_type,omitempty"`
	PrimaryChannel string                         `json:"primary_channel_prefix,omitempty"`
	Concurrency    int                            `json:"concurrency"`
	Status         liveMonitorStatus              `json:"status"`
	UpdatedAt      string                         `json:"updated_at"`
	TypeMismatch   bool                           `json:"type_mismatch"`
	TypeMismatchAt string                         `json:"type_mismatch_at,omitempty"`
	ActiveRequests []liveMonitorActiveRequestItem `json:"active_requests"`
	IssueTimeline  []liveMonitorHistoryEntry      `json:"issue_timeline"`
}

type liveMonitorHistoryPage struct {
	Total  int                       `json:"total"`
	Offset int                       `json:"offset"`
	Limit  int                       `json:"limit"`
	Items  []liveMonitorHistoryEntry `json:"items"`
}

type liveMonitorStreamEnvelope struct {
	Type       string           `json:"type"`
	CardID     string           `json:"card_id,omitempty"`
	TotalCards int              `json:"total_cards"`
	Card       *liveMonitorCard `json:"card,omitempty"`
	At         string           `json:"at"`
}

type liveMonitorRequest struct {
	RequestID            string
	CardKey              string
	ClientIP             string
	RequestedModel       string
	RequestedType        string
	ActualModel          string
	ActualType           string
	CurrentChannelPrefix string
	CurrentChannelLabel  string
	CurrentChannelBase   string
	AuthID               string
	StartedAt            time.Time
	UpdatedAt            time.Time
	FinishedAt           time.Time
	Active               bool
	HasUnresolvedIssue   bool
	ErrorSummary         string
	ErrorReason          string
	HTTPStatus           int
	FinalStatus          string
	History              []liveMonitorPersistedEvent
}

type liveMonitorStore struct {
	dir string
	mu  sync.Mutex
}

type liveMonitorRuntime struct {
	mu          sync.RWMutex
	requests    map[string]*liveMonitorRequest
	cardIndex   map[string]map[string]struct{}
	subscribers map[uint64]chan liveMonitorStreamEnvelope
	nextSubID   atomic.Uint64
	store       *liveMonitorStore
}

func newLiveMonitorRuntime(stateDir string) *liveMonitorRuntime {
	store := &liveMonitorStore{dir: strings.TrimSpace(stateDir)}
	_ = os.MkdirAll(store.dir, 0o755)
	runtime := &liveMonitorRuntime{
		requests:    make(map[string]*liveMonitorRequest),
		cardIndex:   make(map[string]map[string]struct{}),
		subscribers: make(map[uint64]chan liveMonitorStreamEnvelope),
		store:       store,
	}
	runtime.restore(time.Now().UTC())
	return runtime
}

func (r *liveMonitorRuntime) restore(now time.Time) {
	if r == nil || r.store == nil {
		return
	}
	events, err := r.store.load(now.Add(-liveMonitorMaxWindow), now)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, event := range events {
		r.applyEventLocked(event)
	}
	r.pruneLocked(now)
}

func (r *liveMonitorRuntime) recordStart(event liveMonitorPersistedEvent) {
	if r == nil || strings.TrimSpace(event.RequestID) == "" || strings.TrimSpace(event.ClientIP) == "" || strings.TrimSpace(event.RequestedModel) == "" {
		return
	}
	event.Kind = "start"
	r.recordEvent(event)
}

func (r *liveMonitorRuntime) recordRoute(event liveMonitorPersistedEvent) {
	if r == nil || strings.TrimSpace(event.RequestID) == "" {
		return
	}
	event.Kind = "route"
	r.recordEvent(event)
}

func (r *liveMonitorRuntime) recordCompletion(event liveMonitorPersistedEvent) {
	if r == nil || strings.TrimSpace(event.RequestID) == "" {
		return
	}
	event.Kind = "complete"
	r.recordEvent(event)
}

func (r *liveMonitorRuntime) recordEvent(event liveMonitorPersistedEvent) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	} else {
		event.Timestamp = event.Timestamp.UTC()
	}
	if r.store != nil {
		_ = r.store.append(event)
	}

	r.mu.Lock()
	r.applyEventLocked(event)
	r.pruneLocked(event.Timestamp)
	cardKey := r.cardKeyForEventLocked(event)
	card := r.buildCardLocked(cardKey, event.Timestamp)
	totalCards := r.countCardsLocked(event.Timestamp)
	subs := r.snapshotSubscribersLocked()
	r.mu.Unlock()

	if card == nil && cardKey == "" {
		return
	}
	envelope := liveMonitorStreamEnvelope{
		Type:       "card_touch",
		CardID:     encodeLiveMonitorCardID(cardKey),
		TotalCards: totalCards,
		At:         event.Timestamp.Format(time.RFC3339Nano),
		Card:       card,
	}
	for _, ch := range subs {
		select {
		case ch <- envelope:
		default:
		}
	}
}

func (r *liveMonitorRuntime) snapshotSubscribersLocked() []chan liveMonitorStreamEnvelope {
	out := make([]chan liveMonitorStreamEnvelope, 0, len(r.subscribers))
	for _, ch := range r.subscribers {
		if ch != nil {
			out = append(out, ch)
		}
	}
	return out
}

func (r *liveMonitorRuntime) subscribe() (uint64, <-chan liveMonitorStreamEnvelope) {
	if r == nil {
		return 0, nil
	}
	id := r.nextSubID.Add(1)
	ch := make(chan liveMonitorStreamEnvelope, 16)
	r.mu.Lock()
	r.subscribers[id] = ch
	r.mu.Unlock()
	return id, ch
}

func (r *liveMonitorRuntime) unsubscribe(id uint64) {
	if r == nil || id == 0 {
		return
	}
	r.mu.Lock()
	ch := r.subscribers[id]
	delete(r.subscribers, id)
	r.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (r *liveMonitorRuntime) snapshot(rangeWindow time.Duration, page, pageSize int, now time.Time) liveMonitorCardPage {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rangeWindow = normalizeLiveMonitorRange(rangeWindow)
	pageSize = normalizeLiveMonitorPageSize(pageSize)
	if page <= 0 {
		page = 1
	}

	r.mu.Lock()
	r.pruneLocked(now)
	cards := r.collectCardsLocked(now)
	r.mu.Unlock()

	filtered := make([]liveMonitorCard, 0, len(cards))
	cutoff := now.Add(-rangeWindow)
	for _, card := range cards {
		updatedAt, err := time.Parse(time.RFC3339Nano, card.UpdatedAt)
		if err != nil || updatedAt.Before(cutoff) {
			continue
		}
		filtered = append(filtered, card)
	}

	totalCards := len(filtered)
	totalPages := 1
	if totalCards > 0 {
		totalPages = (totalCards + pageSize - 1) / pageSize
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * pageSize
	if start < 0 {
		start = 0
	}
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	pageCards := []liveMonitorCard{}
	if start < len(filtered) {
		pageCards = filtered[start:end]
	}

	return liveMonitorCardPage{
		GeneratedAt: now.Format(time.RFC3339Nano),
		Range:       formatLiveMonitorRange(rangeWindow),
		Page:        page,
		PageSize:    pageSize,
		TotalCards:  totalCards,
		TotalPages:  totalPages,
		Cards:       pageCards,
	}
}

func (r *liveMonitorRuntime) detail(cardID string, rangeWindow time.Duration, now time.Time) (liveMonitorDetail, bool) {
	key, err := decodeLiveMonitorCardID(cardID)
	if err != nil {
		return liveMonitorDetail{}, false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rangeWindow = normalizeLiveMonitorRange(rangeWindow)

	r.mu.Lock()
	r.pruneLocked(now)
	card := r.buildCardLocked(key, now)
	requests := r.requestsForCardLocked(key)
	r.mu.Unlock()
	if card == nil {
		return liveMonitorDetail{}, false
	}

	cutoff := now.Add(-rangeWindow)
	activeItems := make([]liveMonitorActiveRequestItem, 0)
	issueTimeline := make([]liveMonitorHistoryEntry, 0)
	requestedModel := ""
	fullIP := ""
	primaryChannel := card.PrimaryChannelPrefix

	for _, req := range requests {
		if req == nil || req.UpdatedAt.Before(cutoff) {
			continue
		}
		if requestedModel == "" {
			requestedModel = req.RequestedModel
			fullIP = req.ClientIP
		}
		if req.Active {
			activeItems = append(activeItems, liveMonitorActiveRequestItem{
				RequestID:     req.RequestID,
				Status:        string(requestStatusForRequest(req)),
				ChannelPrefix: req.CurrentChannelPrefix,
				StartedAt:     formatTime(req.StartedAt),
				UpdatedAt:     formatTime(req.UpdatedAt),
				ErrorSummary:  firstNonEmpty(req.ErrorReason, req.ErrorSummary),
				ActualModel:   req.ActualModel,
				ActualType:    req.ActualType,
			})
		}
		for idx := len(req.History) - 1; idx >= 0; idx-- {
			event := req.History[idx]
			if event.Timestamp.Before(cutoff) || !isIssueEvent(event) {
				continue
			}
			issueTimeline = append(issueTimeline, historyEntryFromEvent(event))
		}
	}

	sort.Slice(activeItems, func(i, j int) bool {
		leftRunning := activeItems[i].Status == string(liveMonitorStatusOnline)
		rightRunning := activeItems[j].Status == string(liveMonitorStatusOnline)
		if leftRunning != rightRunning {
			return leftRunning
		}
		return activeItems[i].UpdatedAt > activeItems[j].UpdatedAt
	})
	sort.Slice(issueTimeline, func(i, j int) bool {
		return issueTimeline[i].At > issueTimeline[j].At
	})

	return liveMonitorDetail{
		CardID:         card.CardID,
		FullIP:         fullIP,
		RequestedModel: requestedModel,
		DisplayModel:   card.DisplayModel,
		RequestedType:  card.RequestedType,
		ActualType:     card.ActualType,
		PrimaryChannel: primaryChannel,
		Concurrency:    card.Concurrency,
		Status:         card.Status,
		UpdatedAt:      card.UpdatedAt,
		TypeMismatch:   card.TypeMismatch,
		TypeMismatchAt: card.TypeMismatchAt,
		ActiveRequests: activeItems,
		IssueTimeline:  issueTimeline,
	}, true
}

func (r *liveMonitorRuntime) history(cardID string, rangeWindow time.Duration, offset, limit int, now time.Time) (liveMonitorHistoryPage, bool) {
	key, err := decodeLiveMonitorCardID(cardID)
	if err != nil {
		return liveMonitorHistoryPage{}, false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rangeWindow = normalizeLiveMonitorRange(rangeWindow)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = liveMonitorHistoryStep
	}

	r.mu.Lock()
	r.pruneLocked(now)
	requests := r.requestsForCardLocked(key)
	r.mu.Unlock()

	cutoff := now.Add(-rangeWindow)
	entries := make([]liveMonitorHistoryEntry, 0)
	for _, req := range requests {
		if req == nil {
			continue
		}
		for idx := len(req.History) - 1; idx >= 0; idx-- {
			event := req.History[idx]
			if event.Timestamp.Before(cutoff) {
				continue
			}
			entries = append(entries, historyEntryFromEvent(event))
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].At > entries[j].At
	})

	total := len(entries)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	out := []liveMonitorHistoryEntry{}
	if offset < total {
		out = entries[offset:end]
	}

	return liveMonitorHistoryPage{
		Total:  total,
		Offset: offset,
		Limit:  limit,
		Items:  out,
	}, true
}

func (r *liveMonitorRuntime) countCardsLocked(now time.Time) int {
	return len(r.collectCardsLocked(now))
}

func (r *liveMonitorRuntime) collectCardsLocked(now time.Time) []liveMonitorCard {
	out := make([]liveMonitorCard, 0, len(r.cardIndex))
	for key := range r.cardIndex {
		card := r.buildCardLocked(key, now)
		if card != nil {
			out = append(out, *card)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Status != out[j].Status {
			if out[i].Status == liveMonitorStatusOnline {
				return true
			}
			if out[j].Status == liveMonitorStatusOnline {
				return false
			}
			if out[i].Status == liveMonitorStatusError {
				return true
			}
			if out[j].Status == liveMonitorStatusError {
				return false
			}
		}
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out
}

func (r *liveMonitorRuntime) requestsForCardLocked(cardKey string) []*liveMonitorRequest {
	requestIDs := r.cardIndex[cardKey]
	if len(requestIDs) == 0 {
		return nil
	}
	out := make([]*liveMonitorRequest, 0, len(requestIDs))
	for requestID := range requestIDs {
		if req := r.requests[requestID]; req != nil {
			out = append(out, cloneLiveMonitorRequest(req))
		}
	}
	return out
}

func (r *liveMonitorRuntime) buildCardLocked(cardKey string, _ time.Time) *liveMonitorCard {
	if strings.TrimSpace(cardKey) == "" {
		return nil
	}
	requestIDs := r.cardIndex[cardKey]
	if len(requestIDs) == 0 {
		return nil
	}
	requests := make([]*liveMonitorRequest, 0, len(requestIDs))
	for requestID := range requestIDs {
		req := r.requests[requestID]
		if req == nil {
			continue
		}
		requests = append(requests, req)
	}
	if len(requests) == 0 {
		return nil
	}

	sort.Slice(requests, func(i, j int) bool {
		return requests[i].UpdatedAt.After(requests[j].UpdatedAt)
	})

	latest := requests[0]
	concurrency := 0
	issueItems := make([]liveMonitorIssueItem, 0, 3)
	issueEvents := make([]liveMonitorPersistedEvent, 0)
	activeChannels := make(map[string]struct{})
	activePrimary := ""
	hasActiveError := false
	recentResultIcon := ""
	mismatchRequested := ""
	mismatchActual := ""
	mismatchAt := time.Time{}

	for _, req := range requests {
		if req.Active {
			concurrency++
			if req.CurrentChannelPrefix != "" {
				activeChannels[req.CurrentChannelPrefix] = struct{}{}
				if activePrimary == "" {
					activePrimary = req.CurrentChannelPrefix
				}
			}
			if req.HasUnresolvedIssue {
				hasActiveError = true
			}
		}
		if req.RequestedType != "" && req.ActualType != "" && req.RequestedType != req.ActualType && req.UpdatedAt.After(mismatchAt) {
			mismatchRequested = req.RequestedType
			mismatchActual = req.ActualType
			mismatchAt = req.UpdatedAt
		}
		if recentResultIcon == "" && !req.Active {
			recentResultIcon = reqRecentResultIcon(req)
		}
		for idx := len(req.History) - 1; idx >= 0; idx-- {
			event := req.History[idx]
			if !isIssueEvent(event) {
				continue
			}
			issueEvents = append(issueEvents, event)
			if len(issueEvents) >= 3 {
				break
			}
		}
		if len(issueEvents) >= 3 {
			break
		}
	}

	for _, event := range issueEvents {
		issueItems = append(issueItems, liveMonitorIssueItem{
			At:            formatTime(event.Timestamp),
			ChannelPrefix: event.ChannelPrefix,
			Reason:        firstNonEmpty(event.FallbackReason, event.StateTransition),
			ErrorMessage:  event.ErrorMessage,
		})
	}

	status := liveMonitorStatusOffline
	if concurrency > 0 {
		if hasActiveError {
			status = liveMonitorStatusError
		} else {
			status = liveMonitorStatusOnline
		}
	}

	displayModel := latest.ActualModel
	if displayModel == "" {
		displayModel = latest.RequestedModel
	}
	primaryChannel := ""
	extraChannels := 0
	if concurrency > 0 {
		primaryChannel = activePrimary
		if len(activeChannels) > 0 {
			extraChannels = len(activeChannels) - 1
			if extraChannels < 0 {
				extraChannels = 0
			}
		}
	}

	clientIP, requestedModel := splitLiveMonitorCardKey(cardKey)
	return &liveMonitorCard{
		CardID:                encodeLiveMonitorCardID(cardKey),
		MaskedIP:              maskIPv4(clientIP),
		RequestedModel:        requestedModel,
		DisplayModel:          displayModel,
		RequestedType:         latest.RequestedType,
		ActualType:            latest.ActualType,
		PrimaryChannelPrefix:  primaryChannel,
		ExtraChannelCount:     extraChannels,
		Concurrency:           concurrency,
		Status:                status,
		UpdatedAt:             formatTime(latest.UpdatedAt),
		RecentIssues:          issueItems,
		TypeMismatch:          !mismatchAt.IsZero(),
		TypeMismatchRequested: mismatchRequested,
		TypeMismatchActual:    mismatchActual,
		TypeMismatchAt:        formatTime(mismatchAt),
		RecentResultIcon:      recentResultIcon,
	}
}

func (r *liveMonitorRuntime) cardKeyForEventLocked(event liveMonitorPersistedEvent) string {
	if strings.TrimSpace(event.ClientIP) != "" && strings.TrimSpace(event.RequestedModel) != "" {
		return liveMonitorCardKey(event.ClientIP, event.RequestedModel)
	}
	if req := r.requests[strings.TrimSpace(event.RequestID)]; req != nil {
		return req.CardKey
	}
	return ""
}

func (r *liveMonitorRuntime) applyEventLocked(event liveMonitorPersistedEvent) {
	requestID := strings.TrimSpace(event.RequestID)
	if requestID == "" {
		return
	}
	req := r.requests[requestID]
	if req == nil {
		req = &liveMonitorRequest{RequestID: requestID}
		r.requests[requestID] = req
	}
	if !event.Timestamp.IsZero() && event.Timestamp.After(req.UpdatedAt) {
		req.UpdatedAt = event.Timestamp
	}
	switch event.Kind {
	case "start":
		req.ClientIP = firstNonEmpty(req.ClientIP, strings.TrimSpace(event.ClientIP))
		req.RequestedModel = firstNonEmpty(req.RequestedModel, strings.TrimSpace(event.RequestedModel))
		req.RequestedType = firstNonEmpty(req.RequestedType, strings.TrimSpace(event.RequestedType))
		if req.StartedAt.IsZero() || event.Timestamp.Before(req.StartedAt) {
			req.StartedAt = event.Timestamp
		}
		req.Active = true
		req.FinalStatus = ""
		req.CardKey = liveMonitorCardKey(req.ClientIP, req.RequestedModel)
		r.indexRequestLocked(req)
	case "route":
		if req.StartedAt.IsZero() {
			req.StartedAt = event.Timestamp
		}
		req.Active = true
		req.ActualModel = firstNonEmpty(strings.TrimSpace(event.ActualModel), req.ActualModel)
		req.ActualType = firstNonEmpty(strings.TrimSpace(event.ActualType), req.ActualType)
		req.CurrentChannelPrefix = firstNonEmpty(strings.TrimSpace(event.ChannelPrefix), req.CurrentChannelPrefix)
		req.CurrentChannelLabel = firstNonEmpty(strings.TrimSpace(event.ChannelLabel), req.CurrentChannelLabel)
		req.CurrentChannelBase = firstNonEmpty(strings.TrimSpace(event.ChannelBaseURL), req.CurrentChannelBase)
		req.AuthID = firstNonEmpty(strings.TrimSpace(event.AuthID), req.AuthID)
		if event.ResultStatus == "failed" {
			req.HasUnresolvedIssue = true
			req.ErrorReason = firstNonEmpty(strings.TrimSpace(event.FallbackReason), strings.TrimSpace(event.StateTransition), req.ErrorReason)
			req.ErrorSummary = firstNonEmpty(strings.TrimSpace(event.ErrorMessage), req.ErrorSummary)
		}
		if event.ResultStatus == "success" || event.ResultStatus == "selected" {
			req.HasUnresolvedIssue = false
			if event.ResultStatus == "success" {
				req.ErrorReason = ""
				req.ErrorSummary = ""
			}
		}
	case "complete":
		req.Active = false
		req.FinishedAt = event.Timestamp
		req.HTTPStatus = event.HTTPStatus
		if event.HTTPStatus >= 400 || event.ResultStatus == "failed" {
			req.FinalStatus = "failure"
			req.ErrorReason = firstNonEmpty(strings.TrimSpace(event.FallbackReason), strings.TrimSpace(event.StateTransition), req.ErrorReason)
			req.ErrorSummary = firstNonEmpty(strings.TrimSpace(event.ErrorMessage), req.ErrorSummary)
		} else {
			req.FinalStatus = "success"
			req.HasUnresolvedIssue = false
			req.ErrorReason = ""
			req.ErrorSummary = ""
		}
	}
	req.History = append(req.History, event)
}

func (r *liveMonitorRuntime) indexRequestLocked(req *liveMonitorRequest) {
	if req == nil || strings.TrimSpace(req.CardKey) == "" || strings.TrimSpace(req.RequestID) == "" {
		return
	}
	bucket := r.cardIndex[req.CardKey]
	if bucket == nil {
		bucket = make(map[string]struct{})
		r.cardIndex[req.CardKey] = bucket
	}
	bucket[req.RequestID] = struct{}{}
}

func (r *liveMonitorRuntime) pruneLocked(now time.Time) {
	cutoff := now.Add(-liveMonitorMaxWindow)
	for requestID, req := range r.requests {
		if req == nil {
			delete(r.requests, requestID)
			continue
		}
		req.History = pruneLiveMonitorEvents(req.History, cutoff)
		if !req.Active && req.UpdatedAt.Before(cutoff) {
			delete(r.requests, requestID)
			continue
		}
	}
	for cardKey, requestIDs := range r.cardIndex {
		for requestID := range requestIDs {
			if _, ok := r.requests[requestID]; !ok {
				delete(requestIDs, requestID)
			}
		}
		if len(requestIDs) == 0 {
			delete(r.cardIndex, cardKey)
		}
	}
}

func pruneLiveMonitorEvents(events []liveMonitorPersistedEvent, cutoff time.Time) []liveMonitorPersistedEvent {
	if len(events) == 0 {
		return events
	}
	write := 0
	for _, event := range events {
		if event.Timestamp.Before(cutoff) {
			continue
		}
		events[write] = event
		write++
	}
	return events[:write]
}

func cloneLiveMonitorRequest(src *liveMonitorRequest) *liveMonitorRequest {
	if src == nil {
		return nil
	}
	clone := *src
	if len(src.History) > 0 {
		clone.History = append([]liveMonitorPersistedEvent(nil), src.History...)
	}
	return &clone
}

func requestStatusForRequest(req *liveMonitorRequest) liveMonitorStatus {
	if req == nil {
		return liveMonitorStatusOffline
	}
	if req.Active {
		if req.HasUnresolvedIssue {
			return liveMonitorStatusError
		}
		return liveMonitorStatusOnline
	}
	return liveMonitorStatusOffline
}

func reqRecentResultIcon(req *liveMonitorRequest) string {
	if req == nil {
		return ""
	}
	if req.FinalStatus == "failure" {
		return "failure"
	}
	if req.FinalStatus == "success" {
		for _, event := range req.History {
			if event.Kind == "route" && event.ResultStatus == "failed" {
				return "switch"
			}
		}
		return "success"
	}
	return ""
}

func isIssueEvent(event liveMonitorPersistedEvent) bool {
	if event.Kind == "route" && (event.ResultStatus == "failed" || event.ResultStatus == "selected") {
		if event.ResultStatus == "selected" {
			return strings.TrimSpace(event.FallbackReason) != ""
		}
		return true
	}
	if event.Kind == "complete" && event.HTTPStatus >= 400 {
		return true
	}
	return false
}

func historyEntryFromEvent(event liveMonitorPersistedEvent) liveMonitorHistoryEntry {
	reason := firstNonEmpty(event.FallbackReason, event.StateTransition)
	result := firstNonEmpty(event.ResultStatus, event.Kind)
	return liveMonitorHistoryEntry{
		ID:            fmt.Sprintf("%s-%s", event.RequestID, event.Timestamp.UTC().Format(time.RFC3339Nano)),
		At:            formatTime(event.Timestamp),
		Kind:          event.Kind,
		Result:        result,
		ChannelPrefix: event.ChannelPrefix,
		ActualType:    event.ActualType,
		ActualModel:   event.ActualModel,
		AuthID:        event.AuthID,
		Reason:        reason,
		ErrorMessage:  event.ErrorMessage,
		HTTPStatus:    event.HTTPStatus,
		RequestID:     event.RequestID,
	}
}

func normalizeLiveMonitorRange(window time.Duration) time.Duration {
	switch window {
	case 6 * time.Hour, 12 * time.Hour, 24 * time.Hour:
		return window
	default:
		return liveMonitorDefaultRange
	}
}

func formatLiveMonitorRange(window time.Duration) string {
	switch window {
	case 12 * time.Hour:
		return "12h"
	case 24 * time.Hour:
		return "24h"
	default:
		return "6h"
	}
}

func parseLiveMonitorRange(raw string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "12h":
		return 12 * time.Hour
	case "24h":
		return 24 * time.Hour
	default:
		return 6 * time.Hour
	}
}

func normalizeLiveMonitorPageSize(pageSize int) int {
	if pageSize <= 0 {
		return liveMonitorDefaultPageSize
	}
	if pageSize > 50 {
		return 50
	}
	return pageSize
}

func formatTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339Nano)
}

func liveMonitorCardKey(clientIP, requestedModel string) string {
	return strings.TrimSpace(clientIP) + "\n" + strings.TrimSpace(requestedModel)
}

func splitLiveMonitorCardKey(key string) (string, string) {
	parts := strings.SplitN(key, "\n", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func encodeLiveMonitorCardID(key string) string {
	if strings.TrimSpace(key) == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(key))
}

func decodeLiveMonitorCardID(cardID string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cardID))
	if err != nil {
		return "", err
	}
	key := string(raw)
	if !strings.Contains(key, "\n") {
		return "", errors.New("invalid card id")
	}
	return key, nil
}

func maskIPv4(ip string) string {
	parts := strings.Split(strings.TrimSpace(ip), ".")
	if len(parts) != 4 {
		return ip
	}
	return fmt.Sprintf("%s.%s.%s.*", parts[0], parts[1], parts[2])
}

func (s *liveMonitorStore) append(event liveMonitorPersistedEvent) error {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return nil
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	path := filepath.Join(s.dir, event.Timestamp.UTC().Format("2006-01-02")+".jsonl")
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(line); err != nil {
		return err
	}
	if _, err := file.Write([]byte("\n")); err != nil {
		return err
	}
	s.cleanup(event.Timestamp)
	return nil
}

func (s *liveMonitorStore) cleanup(now time.Time) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-liveMonitorRetention)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".jsonl")
		day, err := time.Parse("2006-01-02", name)
		if err != nil || !day.Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(s.dir, entry.Name()))
	}
}

func (s *liveMonitorStore) load(since, until time.Time) ([]liveMonitorPersistedEvent, error) {
	if s == nil || strings.TrimSpace(s.dir) == "" {
		return nil, nil
	}
	if until.IsZero() {
		until = time.Now().UTC()
	}
	if since.IsZero() {
		since = until.Add(-liveMonitorMaxWindow)
	}
	if since.After(until) {
		since, until = until, since
	}
	events := make([]liveMonitorPersistedEvent, 0)
	startDay := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	endDay := time.Date(until.Year(), until.Month(), until.Day(), 0, 0, 0, 0, time.UTC)
	for day := startDay; !day.After(endDay); day = day.Add(24 * time.Hour) {
		path := filepath.Join(s.dir, day.Format("2006-01-02")+".jsonl")
		file, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return events, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var event liveMonitorPersistedEvent
			if err := json.Unmarshal(line, &event); err != nil {
				_ = file.Close()
				return events, err
			}
			event.Timestamp = event.Timestamp.UTC()
			if event.Timestamp.Before(since) || event.Timestamp.After(until) {
				continue
			}
			events = append(events, event)
		}
		if err := scanner.Err(); err != nil {
			_ = file.Close()
			return events, err
		}
		_ = file.Close()
	}
	sort.Slice(events, func(i, j int) bool {
		return events[i].Timestamp.Before(events[j].Timestamp)
	})
	return events, nil
}
