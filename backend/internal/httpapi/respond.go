package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sovereigntrust/wallet/internal/apperr"
	"github.com/sovereigntrust/wallet/internal/reqctx"
)

type errorBody struct {
	Error struct {
		Code      apperr.Code       `json:"code"`
		Message   string            `json:"message"`
		Fields    map[string]string `json:"fields,omitempty"`
		RequestID string            `json:"request_id,omitempty"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var body errorBody
	body.Error.RequestID = reqctx.MetaFrom(r.Context()).RequestID
	status := http.StatusInternalServerError
	if e, ok := apperr.As(err); ok {
		status = e.HTTPStatus()
		body.Error.Code, body.Error.Message, body.Error.Fields = e.Code, e.Message, e.Fields
	} else {
		// Never leak internal details; log them with the request ID instead.
		s.Log.Error("unhandled error", "err", err, "request_id", body.Error.RequestID, "path", r.URL.Path)
		body.Error.Code, body.Error.Message = apperr.CodeInternal, "an internal error occurred"
	}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "60")
	}
	writeJSON(w, status, body)
}

const maxBody = 64 << 10

// decode strictly parses a JSON body.
func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			return apperr.Validation("request body too large", nil)
		case errors.Is(err, io.EOF):
			return apperr.Validation("request body required", nil)
		default:
			msg := err.Error()
			if strings.HasPrefix(msg, "json: unknown field") {
				return apperr.Validation(strings.TrimPrefix(msg, "json: "), nil)
			}
			return apperr.Validation("malformed JSON body", nil)
		}
	}
	if dec.More() {
		return apperr.Validation("request body must contain a single JSON object", nil)
	}
	return nil
}

func pathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, apperr.NotFound(strings.TrimSuffix(name, "ID"))
	}
	return id, nil
}

func principal(r *http.Request) reqctx.Principal {
	p, _ := reqctx.PrincipalFrom(r.Context())
	return p
}

func idempotencyKey(r *http.Request) (string, error) {
	k := r.Header.Get("Idempotency-Key")
	if k == "" {
		return "", apperr.New(apperr.CodeIdempotencyRequired, "Idempotency-Key header is required for this operation")
	}
	return k, nil
}

func created(w http.ResponseWriter, replay bool, v any, status int) {
	if replay {
		w.Header().Set("Idempotent-Replayed", "true")
		status = http.StatusOK
	}
	writeJSON(w, status, v)
}

// Cursor-based pagination over (created_at, id).
type cursor struct {
	T  time.Time
	ID uuid.UUID
}

func encodeCursor(t time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d|%s", t.UnixNano(), id)))
}

func decodeCursor(s string) (*cursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, apperr.Field("cursor", "invalid cursor")
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 {
		return nil, apperr.Field("cursor", "invalid cursor")
	}
	var ns int64
	if _, err := fmt.Sscanf(parts[0], "%d", &ns); err != nil {
		return nil, apperr.Field("cursor", "invalid cursor")
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return nil, apperr.Field("cursor", "invalid cursor")
	}
	return &cursor{T: time.Unix(0, ns), ID: id}, nil
}

func queryInt(r *http.Request, name string, def int) int {
	var v int
	if _, err := fmt.Sscanf(r.URL.Query().Get(name), "%d", &v); err != nil || v <= 0 {
		return def
	}
	return v
}
