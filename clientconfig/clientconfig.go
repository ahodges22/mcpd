// Package clientconfig plans and conditionally applies supported MCP client edits.
package clientconfig

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ahodges22/mcpd/internal/atomicfile"
	"github.com/ahodges22/mcpd/internal/install"
)

type ConflictError struct{ Path string }

func (e *ConflictError) Error() string { return e.Path + " changed since it was inspected" }

type Plan struct {
	Client      string   `json:"client"`
	Path        string   `json:"path"`
	Endpoint    string   `json:"endpoint"`
	Notes       []string `json:"notes,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	SourceHash  string   `json:"source_hash"`
	ReceiptHash string   `json:"receipt_hash,omitempty"`

	kind                        string
	state                       string
	client                      install.Client
	inner                       install.Plan
	before, after               []byte
	receiptPath                 string
	receiptBefore, receiptAfter []byte
}

func (p Plan) Empty() bool { return p.kind == "noop" || (len(p.before) == 0 && len(p.after) == 0) }

func PlanInstall(home, state, client, addr string) (Plan, error) {
	c, err := install.Lookup(home, client)
	if err != nil {
		return Plan{}, err
	}
	p, err := c.PlanInstall(addr)
	if err != nil {
		return Plan{}, err
	}
	before, after, err := p.Preview()
	if err != nil {
		return Plan{}, err
	}
	return Plan{Client: c.Name, Path: c.Path, Endpoint: p.Endpoint, Notes: p.Notes, Warnings: p.Warnings,
		SourceHash: hash(before), kind: "install", state: state, client: c, inner: p, before: before, after: after}, nil
}

func PlanRevert(home, state, client string) (Plan, error) {
	c, err := install.Lookup(home, client)
	if err != nil {
		return Plan{}, err
	}
	p, err := c.PlanRevert(state)
	if err != nil {
		return Plan{}, err
	}
	before, after, err := p.Preview()
	if err != nil {
		return Plan{}, err
	}
	return Plan{Client: c.Name, Path: c.Path, Endpoint: p.Endpoint, Notes: p.Notes,
		SourceHash: hash(before), kind: "revert", state: state, client: c, inner: p, before: before, after: after}, nil
}

// PlanRetarget plans the installed mcpd entry and receipt from one address to another.
func PlanRetarget(home, state, client, fromAddr, toAddr string) (Plan, error) {
	c, err := install.Lookup(home, client)
	if err != nil {
		return Plan{}, err
	}
	before, err := os.ReadFile(c.Path)
	if err != nil {
		return Plan{}, fmt.Errorf("read %s: %w", c.Path, err)
	}
	oldEndpoint, newEndpoint := c.Endpoint(fromAddr), c.Endpoint(toAddr)
	oldCount := strings.Count(string(before), oldEndpoint)
	newCount := strings.Count(string(before), newEndpoint)
	if (oldCount != 1 || newCount != 0) && (oldCount != 0 || newCount != 1) {
		return Plan{}, fmt.Errorf("%w: expected one %s or %s endpoint in %s, found %d and %d",
			install.ErrConflict, oldEndpoint, newEndpoint, c.Path, oldCount, newCount)
	}
	after := before
	if oldCount == 1 {
		after = []byte(strings.Replace(string(before), oldEndpoint, newEndpoint, 1))
	}
	rp := filepath.Join(state, "install", c.Name+".json")
	rawReceipt, err := os.ReadFile(rp)
	if err != nil {
		return Plan{}, fmt.Errorf("read receipt: %w", err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(rawReceipt, &receipt); err != nil {
		return Plan{}, fmt.Errorf("parse receipt: %w", err)
	}
	endpoint, _ := receipt["endpoint"].(string)
	if endpoint != oldEndpoint && endpoint != newEndpoint {
		return Plan{}, fmt.Errorf("%w: receipt endpoint is %q, want %q or %q", install.ErrConflict, endpoint, oldEndpoint, newEndpoint)
	}
	if oldCount == 1 && endpoint != oldEndpoint {
		return Plan{}, fmt.Errorf("%w: client endpoint is %q but receipt endpoint is %q", install.ErrConflict, oldEndpoint, endpoint)
	}
	receiptChanged := endpoint == oldEndpoint
	receipt["endpoint"] = newEndpoint
	if edits, ok := receipt["edits"].([]any); ok {
		for _, rawEdit := range edits {
			edit, ok := rawEdit.(map[string]any)
			if !ok {
				continue
			}
			for _, field := range []string{"From", "To"} {
				if value, ok := edit[field].(string); ok {
					rewritten := strings.ReplaceAll(value, oldEndpoint, newEndpoint)
					if rewritten != value {
						receiptChanged = true
						edit[field] = rewritten
					}
				}
			}
		}
	}
	if oldCount == 0 && !receiptChanged {
		return Plan{Client: c.Name, Path: c.Path, Endpoint: newEndpoint, SourceHash: hash(before), kind: "noop", client: c, before: before, after: before}, nil
	}
	nextReceipt, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return Plan{}, err
	}
	nextReceipt = append(nextReceipt, '\n')
	return Plan{Client: c.Name, Path: c.Path, Endpoint: newEndpoint,
		Notes:      []string{"retarget the mcpd entry from " + oldEndpoint + " to " + newEndpoint},
		SourceHash: hash(before), ReceiptHash: hash(rawReceipt), kind: "retarget", state: state,
		client: c, before: before, after: after, receiptPath: rp, receiptBefore: rawReceipt, receiptAfter: nextReceipt}, nil
}

func Apply(ctx context.Context, p Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.kind == "noop" {
		return nil
	}
	if p.kind == "retarget" {
		return applyRetarget(p)
	}
	if p.kind != "install" {
		return errors.New("plan is not an install or retarget plan")
	}
	current, err := os.ReadFile(p.Path)
	if err != nil {
		return err
	}
	if string(current) == string(p.after) {
		return nil
	}
	if string(current) != string(p.before) {
		return &ConflictError{Path: p.Path}
	}
	if err := p.client.Apply(p.state, p.inner); err != nil {
		if strings.Contains(err.Error(), "changed since it was inspected") {
			return &ConflictError{Path: p.Path}
		}
		return err
	}
	return nil
}

func Revert(ctx context.Context, p Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.kind != "revert" {
		return errors.New("plan is not a revert plan")
	}
	current, err := os.ReadFile(p.Path)
	if err != nil {
		return err
	}
	if string(current) == string(p.after) {
		if p.inner.Empty() {
			return p.client.Revert(p.state, p.inner)
		}
		return nil
	}
	if string(current) != string(p.before) {
		return &ConflictError{Path: p.Path}
	}
	if err := p.client.Revert(p.state, p.inner); err != nil {
		if strings.Contains(err.Error(), "changed since it was inspected") {
			return &ConflictError{Path: p.Path}
		}
		return err
	}
	return nil
}

func applyRetarget(p Plan) error {
	current, err := os.ReadFile(p.Path)
	if err != nil {
		return err
	}
	currentReceipt, err := os.ReadFile(p.receiptPath)
	if err != nil {
		return err
	}
	if string(current) == string(p.after) && string(currentReceipt) == string(p.receiptAfter) {
		return nil
	}
	if string(current) != string(p.before) && string(current) != string(p.after) {
		return &ConflictError{Path: p.Path}
	}
	if string(currentReceipt) != string(p.receiptBefore) {
		return &ConflictError{Path: p.receiptPath}
	}
	if string(p.before) != string(p.after) && string(current) == string(p.before) {
		info, err := os.Stat(p.Path)
		if err != nil {
			return err
		}
		if err := p.client.PrepareMutation(p.before, p.after); err != nil {
			return err
		}
		if err := atomicfile.Write(p.Path, p.after, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return atomicfile.Write(p.receiptPath, p.receiptAfter, 0o600)
}

func hash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
