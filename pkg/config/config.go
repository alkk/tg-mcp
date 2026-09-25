// Package config loads the chat map: the allowlist of telegram chats tg-mcp is allowed to
// log and reply to, keyed by chat id and resolved to a customer slug, and optionally to the
// public alias a chat's newest message is served under without authentication.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"

	"gopkg.in/yaml.v3"
)

var (
	publicRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	usernameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{3,31}$`)
)

// ChatInfo describes a single allowlisted chat.
type ChatInfo struct {
	Customer string `yaml:"customer"`
	Label    string `yaml:"label"`
	Public   string `yaml:"public"`
	Username string `yaml:"username"`
}

// Chat is a ChatInfo together with its chat id.
type Chat struct {
	ID int64
	ChatInfo
}

// Config is the loaded chat map.
type Config struct {
	chats map[int64]ChatInfo
}

type file struct {
	Chats map[int64]ChatInfo `yaml:"chats"`
}

// Load reads and validates the chat map from a YAML file.
func Load(path string) (*Config, error) {
	fh, err := os.Open(path) //nolint:gosec // path comes from the operator via a flag
	if err != nil {
		return nil, fmt.Errorf("open chat map %q: %w", path, err)
	}
	defer fh.Close()

	var f file
	dec := yaml.NewDecoder(fh)
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) { // EOF means an empty file
		return nil, fmt.Errorf("parse chat map %q: %w", path, err)
	}

	cfg := &Config{chats: f.Chats}
	if cfg.chats == nil {
		cfg.chats = map[int64]ChatInfo{}
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid chat map %q: %w", path, err)
	}
	return cfg, nil
}

// validate rejects empty customer slugs and labels repeated within one customer, and requires a
// label on every chat of a customer owning more than one: no label value selects an unlabeled
// chat, so it would be ingested yet unreachable by any addressed tool. Public aliases are one url
// namespace, so they are unique across the whole map, not per customer; a username only feeds the
// public link, so one without an alias is rejected as a dead setting.
func (c *Config) validate() error {
	count := map[string]int{}
	for _, info := range c.chats {
		count[info.Customer]++
	}

	aliases := map[string]int64{}
	seen := map[string]map[string]int64{}
	for _, id := range c.ids() {
		info := c.chats[id]
		if info.Customer == "" {
			return fmt.Errorf("chat %d: empty customer slug", id)
		}
		if info.Label == "" && count[info.Customer] > 1 {
			return fmt.Errorf("customer %q: chat %d has no label, "+
				"a customer with several chats needs a unique label on each", info.Customer, id)
		}
		labels, ok := seen[info.Customer]
		if !ok {
			labels = map[string]int64{}
			seen[info.Customer] = labels
		}
		if other, dup := labels[info.Label]; dup {
			return fmt.Errorf("customer %q: label %q used by both chat %d and %d", info.Customer, info.Label, other, id)
		}
		labels[info.Label] = id

		if info.Public != "" {
			if !publicRe.MatchString(info.Public) {
				return fmt.Errorf("chat %d: public alias %q must match %s", id, info.Public, publicRe)
			}
			if other, dup := aliases[info.Public]; dup {
				return fmt.Errorf("public alias %q used by both chat %d and %d", info.Public, other, id)
			}
			aliases[info.Public] = id
		}
		if info.Username != "" {
			if !usernameRe.MatchString(info.Username) {
				return fmt.Errorf("chat %d: username %q must match %s", id, info.Username, usernameRe)
			}
			if info.Public == "" {
				return fmt.Errorf("chat %d: username %q set without a public alias", id, info.Username)
			}
		}
	}
	return nil
}

// ByChat returns the chat info for a chat id; ok is false for chats outside the allowlist.
func (c *Config) ByChat(chatID int64) (info ChatInfo, ok bool) {
	info, ok = c.chats[chatID]
	return info, ok
}

// ByPublic returns the chat exposed under a public alias; an empty name never matches.
func (c *Config) ByPublic(name string) (Chat, bool) {
	if name == "" {
		return Chat{}, false
	}
	for id, info := range c.chats {
		if info.Public == name {
			return Chat{ID: id, ChatInfo: info}, true
		}
	}
	return Chat{}, false
}

// ByCustomer returns every chat belonging to a customer, ordered by label.
func (c *Config) ByCustomer(customer string) []Chat {
	var res []Chat
	for id, info := range c.chats {
		if info.Customer == customer {
			res = append(res, Chat{ID: id, ChatInfo: info})
		}
	}
	sort.Slice(res, func(i, j int) bool {
		if res[i].Label != res[j].Label {
			return res[i].Label < res[j].Label
		}
		return res[i].ID < res[j].ID
	})
	return res
}

// Customers returns all customer slugs in the chat map, sorted.
func (c *Config) Customers() []string {
	set := map[string]struct{}{}
	for _, info := range c.chats {
		set[info.Customer] = struct{}{}
	}
	res := make([]string, 0, len(set))
	for slug := range set {
		res = append(res, slug)
	}
	sort.Strings(res)
	return res
}

// All returns every allowlisted chat, ordered by customer then label.
func (c *Config) All() []Chat {
	res := make([]Chat, 0, len(c.chats))
	for id, info := range c.chats {
		res = append(res, Chat{ID: id, ChatInfo: info})
	}
	sort.Slice(res, func(i, j int) bool {
		switch {
		case res[i].Customer != res[j].Customer:
			return res[i].Customer < res[j].Customer
		case res[i].Label != res[j].Label:
			return res[i].Label < res[j].Label
		default:
			return res[i].ID < res[j].ID
		}
	})
	return res
}

// ids returns chat ids sorted, so validation errors are deterministic.
func (c *Config) ids() []int64 {
	res := make([]int64, 0, len(c.chats))
	for id := range c.chats {
		res = append(res, id)
	}
	slices.Sort(res)
	return res
}
