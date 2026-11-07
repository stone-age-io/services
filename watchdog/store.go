package watchdog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// stateJSON is a target as stored in the state bucket and returned by
// watchdog.status.
type stateJSON struct {
	Check    string     `json:"check"`
	Target   string     `json:"target"`
	State    State      `json:"state"`
	Since    time.Time  `json:"since"`
	LastSeen *time.Time `json:"last_seen"` // null: never reported
}

func toJSON(t Target) stateJSON {
	s := stateJSON{Check: t.Check, Target: t.Target, State: t.State, Since: t.Since.UTC()}
	if !t.LastSeen.IsZero() {
		ls := t.LastSeen.UTC()
		s.LastSeen = &ls
	}
	return s
}

func fromJSON(data []byte) (Target, error) {
	var s stateJSON
	if err := json.Unmarshal(data, &s); err != nil {
		return Target{}, err
	}
	switch s.State {
	case Pending, Up, Down:
	default:
		return Target{}, fmt.Errorf("unknown state %q", s.State)
	}
	t := Target{Check: s.Check, Target: s.Target, State: s.State, Since: s.Since}
	if s.LastSeen != nil {
		t.LastSeen = *s.LastSeen
	}
	return t, nil
}

// stateKey is the state bucket key for a target.
func stateKey(checkName, targetName string) string { return checkName + "." + targetName }

// splitStateKey reverses stateKey. Check names hold no dots, so the first dot
// is the split.
func splitStateKey(key string) (checkName, targetName string, ok bool) {
	return strings.Cut(key, ".")
}

// Bucket binds a KV bucket, creating it if it is missing. An existing bucket
// is never modified.
func Bucket(ctx context.Context, js jetstream.JetStream, name, description string) (jetstream.KeyValue, error) {
	kv, err := js.KeyValue(ctx, name)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: name, Description: description, History: 1})
	}
	if err != nil {
		return nil, fmt.Errorf("bucket %s: %w", name, err)
	}
	return kv, nil
}

func putState(ctx context.Context, kv jetstream.KeyValue, t Target) error {
	data, err := json.Marshal(toJSON(t))
	if err != nil {
		return err
	}
	_, err = kv.Put(ctx, stateKey(t.Check, t.Target), data)
	return err
}

// purgeCheckState purges every state key of a check.
func purgeCheckState(ctx context.Context, kv jetstream.KeyValue, checkName string) (int, error) {
	lister, err := kv.ListKeysFiltered(ctx, checkName+".*")
	if err != nil {
		return 0, err
	}
	var keys []string
	for k := range lister.Keys() { // the lister stops itself once drained
		keys = append(keys, k)
	}
	for _, k := range keys {
		if err := kv.Purge(ctx, k); err != nil {
			return 0, fmt.Errorf("purge %s: %w", k, err)
		}
	}
	return len(keys), nil
}
