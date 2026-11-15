package feed

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// FollowingCursor preserves the database timestamp and the tie-breaking ID.
// Clients pass the encoded cursor back without parsing timestamps or numeric IDs.
type FollowingCursor struct {
	Version    int       `json:"v"`
	CreateTime time.Time `json:"time"`
	ID         uint      `json:"id,string"`
}

func (cursor FollowingCursor) Encode() string {
	cursor.Version = 1
	cursor.CreateTime = cursor.CreateTime.UTC()
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeFollowingCursor(encoded string) (*FollowingCursor, error) {
	if encoded == "" {
		return nil, nil
	}
	invalid := errors.New("invalid following cursor")
	if len(encoded) > 512 {
		return nil, invalid
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, invalid
	}
	var cursor FollowingCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return nil, invalid
	}
	if cursor.Version != 1 || cursor.ID == 0 || cursor.CreateTime.IsZero() {
		return nil, invalid
	}
	return &cursor, nil
}
