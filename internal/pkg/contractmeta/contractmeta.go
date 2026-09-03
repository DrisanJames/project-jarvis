// Package contractmeta is the standard metadata block every contract in the
// platform carries (REQ-118 §1.5, operator 2026-09-03).
//
// It is deliberately generic — the drip supply chain is the first user, and the
// board, Kumo and click-funnel contract systems are expected to reuse it
// unchanged — so nothing here knows what a "lane" or a "sending domain" means.
// The block answers four questions about a contract row:
//
//	contract_id/kind/version  which row this is
//	refs                      what live objects it points at, resolved at issue time
//	mutation                  who changed it, when, under which change-ledger id
//	token                     an HMAC over the contract's POLICY body, so a
//	                          hand-edited row cannot be honoured
//
// The token is the load-bearing part. `value = HMAC-SHA256(key,
// Canonical(body, kind, subject, version))` with the key from CONTRACT_TOKEN_KEY.
// A contract is issued a token only when it moves approved → scheduled, and the
// mediator verifies it before honouring the contract. Everything here fails
// closed: no key means no token issued and no contract verified.
package contractmeta

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"
)

// Alg names are the supported token algorithms. Only one exists; the field is
// carried so a future rotation to a different primitive is a data change and
// not a silent re-interpretation of old rows.
const (
	AlgHMACSHA256 = "hmac-sha256"
	IssuerSystem  = "system"
)

// Kinds this package has seen. It does NOT validate against them — a contract
// system owns its own kind vocabulary — they exist so the JSON stays readable.
const (
	KindDomain    = "domain"
	KindDispatch  = "dispatch"
	KindInventory = "inventory"
	KindSource    = "source"
)

// Refs are the live objects a contract points at, resolved at issue time and
// re-validated at activation (§1.5 rule 3). Every id is a string so this package
// does not care whether a table keys on a uuid or on a natural key — see
// ResolveOwnedDomainID for a case where it is a natural key.
type Refs struct {
	// SendingDomainID is mailing_sending_profiles.id (domain contracts).
	SendingDomainID string `json:"sending_domain_id,omitempty"`
	// OwnedDomainID identifies the mailing_owned_domains row. That table keys
	// on `domain TEXT` and has no id column, so this carries the domain root.
	OwnedDomainID string `json:"owned_domain_id,omitempty"`
	// DatasetIDs are partner_datasets.id (dispatch/inventory/source contracts).
	DatasetIDs []string `json:"dataset_ids,omitempty"`
	// SegmentIDs are mailing_segments.id — the audience segments the subject
	// mails from / into. Set by the caller; this package has no lookup for it.
	SegmentIDs []string `json:"segment_ids,omitempty"`
}

// Mutation mirrors the change ledger for this version (§1.5 rule 4).
type Mutation struct {
	At             time.Time `json:"at"`
	By             string    `json:"by"`
	ChangeLedgerID string    `json:"change_ledger_id"`
	PriorVersion   int       `json:"prior_version"`
}

// Token is the integrity stamp. Value is hex-encoded HMAC-SHA256.
type Token struct {
	Alg      string    `json:"alg"`
	IssuedAt time.Time `json:"issued_at"`
	IssuedBy string    `json:"issued_by"`
	Value    string    `json:"value"`
}

// Issued reports whether a token has actually been stamped. A zero Token is
// what Issue returns when there is no key, so this is the fail-closed check.
func (t Token) Issued() bool { return t.Value != "" && t.Alg != "" }

// Block is the whole `metadata` JSONB column.
type Block struct {
	ContractID string   `json:"contract_id,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Version    int      `json:"version,omitempty"`
	Refs       Refs     `json:"refs"`
	Mutation   Mutation `json:"mutation"`
	Token      Token    `json:"token"`
}

// Scan implements sql.Scanner for the JSONB column. A NULL, empty or '{}'
// value scans to the zero Block — which has no token and therefore fails
// verification, which is the correct reading of "this row was never issued".
func (b *Block) Scan(src any) error {
	var raw []byte
	switch v := src.(type) {
	case nil:
		*b = Block{}
		return nil
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("contractmeta: cannot scan %T into Block", src)
	}
	if len(raw) == 0 {
		*b = Block{}
		return nil
	}
	var out Block
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("contractmeta: decode metadata block: %w", err)
	}
	*b = out
	return nil
}

// Value implements driver.Valuer, writing the block as JSON text.
func (b Block) Value() (driver.Value, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("contractmeta: encode metadata block: %w", err)
	}
	return string(raw), nil
}

// StampMutation records who changed the contract and under which ledger id.
func (b *Block) StampMutation(at time.Time, by, changeLedgerID string, priorVersion int) {
	b.Mutation = Mutation{
		At:             at.UTC(),
		By:             by,
		ChangeLedgerID: changeLedgerID,
		PriorVersion:   priorVersion,
	}
}

// StampIdentity records which row the block belongs to.
func (b *Block) StampIdentity(contractID, kind string, version int) {
	b.ContractID = contractID
	b.Kind = kind
	b.Version = version
}
