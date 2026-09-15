package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"devbox/internal/packet"
)

// sessionRequest is the canonical form of one start, and it is what the packet's
// source digest covers.
type sessionRequest struct {
	Box          string   `json:"box"`
	Provider     string   `json:"provider"`
	Tree         string   `json:"tree"`
	Dir          string   `json:"dir"`
	Task         string   `json:"task"`
	CurrentState string   `json:"current_state"`
	NextAction   string   `json:"next_action"`
	Constraints  []string `json:"constraints"`
}

// sourceDigest is the digest the handoff packet carries for a session.
//
// An agent session transfers no working tree of its own: the tree slice pushes
// the tree, and this packet carries the session request. The digest therefore
// authenticates the canonical request devbox wrote, so whoever holds the packet
// can tell whether it is the packet devbox produced.
func sourceDigest(request request) (string, error) {
	encoded, err := json.Marshal(sessionRequest{
		Box:          string(request.Box),
		Provider:     string(request.Provider),
		Tree:         request.Tree,
		Dir:          request.Dir,
		Task:         request.Task,
		CurrentState: request.CurrentState,
		NextAction:   request.NextAction,
		Constraints:  request.Constraints,
	})
	if err != nil {
		return "", fmt.Errorf("encode session request: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// buildHandoff assembles the immutable packet for one start.
func buildHandoff(request request) (packet.Handoff, error) {
	digest, err := sourceDigest(request)
	if err != nil {
		return packet.Handoff{}, err
	}
	return packet.Create(packet.CreateInput{
		Box:                  string(request.Box),
		SourceManifestSHA256: digest,
		Task:                 request.Task,
		CurrentState:         request.CurrentState,
		NextAction:           request.NextAction,
		Constraints:          request.Constraints,
	})
}

// writeHandoff stores the operator's copy of the packet.
//
// The packet is uploaded in the same command that writes it, so it carries no
// separate durability requirement: the record, not the packet, is what has to
// survive a dropped connection, and the record store fsyncs.
func writeHandoff(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write handoff %s: %w", path, err)
	}
	return nil
}
