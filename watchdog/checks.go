package watchdog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// DefaultMaxTargets bounds how many targets one check tracks when the check
// does not say. It stops a typo like ">" from learning the whole bus.
const DefaultMaxTargets = 1000

var (
	checkNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	// The characters a NATS KV key may contain.
	keyTokenRe = regexp.MustCompile(`^[-/_=.a-zA-Z0-9]+$`)
)

// Check is one parsed, validated entry from the checks bucket.
type Check struct {
	Name        string
	Subject     string
	TargetToken int // -1 when the subject has no wildcard
	Timeout     time.Duration
	Expected    []string
	MaxTargets  int
}

// checkJSON is the value stored in the checks bucket.
type checkJSON struct {
	Subject     string   `json:"subject"`
	TargetToken *int     `json:"target_token,omitempty"`
	Timeout     string   `json:"timeout"`
	Expected    []string `json:"expected,omitempty"`
	MaxTargets  *int     `json:"max_targets,omitempty"`
}

// ParseCheck parses and validates a check. The error lists every problem
// found, so one round trip through watchdog.validate shows them all.
func ParseCheck(name string, data []byte, evalInterval time.Duration) (Check, error) {
	var errs []error
	if !ValidCheckName(name) {
		errs = append(errs, fmt.Errorf("check name %q must match [a-zA-Z0-9_-]+", name))
	}

	var cj checkJSON
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // a misspelled field is an error, not a silent default
	if err := dec.Decode(&cj); err != nil {
		return Check{}, errors.Join(append(errs, fmt.Errorf("invalid JSON: %w", err))...)
	}

	c := Check{Name: name, Subject: cj.Subject, TargetToken: -1, Expected: cj.Expected, MaxTargets: DefaultMaxTargets}

	tokens, err := splitSubject(cj.Subject)
	if err != nil {
		errs = append(errs, err)
	}
	hasWildcard := false
	for _, t := range tokens {
		if t == "*" || t == ">" {
			hasWildcard = true
		}
	}
	switch {
	case cj.TargetToken == nil && hasWildcard:
		errs = append(errs, errors.New("target_token is required when subject has a wildcard"))
	case cj.TargetToken != nil && !hasWildcard && err == nil:
		errs = append(errs, errors.New("target_token must be omitted when subject has no wildcard"))
	case cj.TargetToken != nil && err == nil:
		i := *cj.TargetToken
		if i < 0 || i >= len(tokens) || tokens[i] != "*" {
			errs = append(errs, fmt.Errorf("target_token %d must point at a '*' token in %q", i, cj.Subject))
		} else {
			c.TargetToken = i
		}
	}

	if cj.Timeout == "" {
		errs = append(errs, errors.New("timeout is required"))
	} else if d, err := time.ParseDuration(cj.Timeout); err != nil {
		errs = append(errs, fmt.Errorf("timeout: %w", err))
	} else if d < 3*evalInterval {
		errs = append(errs, fmt.Errorf("timeout %s must be at least 3x the evaluation interval (%s)", d, 3*evalInterval))
	} else {
		c.Timeout = d
	}

	if cj.MaxTargets != nil {
		if *cj.MaxTargets < 1 {
			errs = append(errs, errors.New("max_targets must be at least 1"))
		} else {
			c.MaxTargets = *cj.MaxTargets
		}
	}
	if len(cj.Expected) > c.MaxTargets {
		errs = append(errs, fmt.Errorf("expected lists %d targets, more than max_targets (%d)", len(cj.Expected), c.MaxTargets))
	}
	if !hasWildcard && len(cj.Expected) > 0 {
		errs = append(errs, errors.New("expected must be omitted when subject has no wildcard"))
	}
	for _, e := range cj.Expected {
		if !ValidTarget(e) {
			errs = append(errs, fmt.Errorf("expected target %q has characters a KV key cannot hold", e))
		}
	}

	if len(errs) > 0 {
		return Check{}, errors.Join(errs...)
	}
	return c, nil
}

// Target returns the target a message subject belongs to. A check without a
// wildcard has one target, named after the check.
func (c Check) Target(subject string) string {
	if c.TargetToken < 0 {
		return c.Name
	}
	tokens := strings.Split(subject, ".")
	if c.TargetToken >= len(tokens) {
		return ""
	}
	return tokens[c.TargetToken]
}

// ValidCheckName reports whether name can be a check name. Check names hold
// no dots, so a {check}.{target} state key always splits cleanly.
func ValidCheckName(name string) bool { return checkNameRe.MatchString(name) }

// ValidTarget reports whether a target can be stored in a KV key. Targets
// come from one subject token, so they hold no dots either.
func ValidTarget(t string) bool { return keyTokenRe.MatchString(t) && !strings.Contains(t, ".") }

// splitSubject checks a subscription subject and returns its tokens.
func splitSubject(s string) ([]string, error) {
	if s == "" {
		return nil, errors.New("subject is required")
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return nil, fmt.Errorf("subject %q contains whitespace", s)
	}
	tokens := strings.Split(s, ".")
	for i, t := range tokens {
		if t == "" {
			return nil, fmt.Errorf("subject %q has an empty token", s)
		}
		if t == ">" && i != len(tokens)-1 {
			return nil, fmt.Errorf("subject %q: '>' must be the last token", s)
		}
		if len(t) > 1 && strings.ContainsAny(t, "*>") {
			return nil, fmt.Errorf("subject %q: wildcards must be a whole token", s)
		}
	}
	return tokens, nil
}
