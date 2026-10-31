package broker

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Intent is one resolved source-preparation intent the broker admits on
// its own node. Every field is resolved on the control-plane side — live
// Prefetch identity, artifact digest, positive resolved size, the
// binding's grant, the node-local manifest path — so the broker never
// accepts a grant, size, or identity from a worker or client message.
type Intent struct {
	// Key is the intent's identity plus revision; one revision admits
	// exactly one reservation, and a changed revision drains the old one.
	Key string
	// Namespace is the entitlement owner of the reservation.
	Namespace string
	// Digest is the artifact manifest digest, "sha256:<hex>".
	Digest string
	// TotalBytes is the artifact's resolved byte total; it must be positive.
	TotalBytes int64
	// Grant is the namespace's administrator grant, required and positive.
	Grant Grant
	// WorkerID names the node's transport worker that fills and serves.
	WorkerID string
	NodeName string
	// IntentUID and IntentGeneration are the intent's immutable identity
	// (its UID and spec generation), part of the stable intent key and of
	// the Binding-wide reservation binding.
	IntentUID        string
	IntentGeneration int64
	IntentName       string
	ArtifactName     string
	// BindingUID, BindingNamespace, and BindingName resolve the intent to
	// its own Binding-wide authority; Registrations is the generation's
	// actual registration plan. ReservationID is optional: when empty the
	// broker mints one and persists it in the prepared record, so a replay
	// of the same revision re-adopts the same reservation.
	BindingUID       string
	BindingNamespace string
	BindingName      string
	ReservationID    string
	Registrations    int32
	// The assignment's identity facts, all resolved from the live
	// ModelArtifact and the verified published tree, never from a worker
	// message: ArtifactUID and SourceGeneration are the manifest identity;
	// ManifestSHA256 is the raw manifest digest and ManifestJSON the exact
	// encoded bytes the worker fills from; TreePath is the published tree;
	// Backend names the vendor backend the assignment drives.
	ArtifactUID      string
	SourceGeneration int64
	ManifestSHA256   string
	ManifestJSON     string
	// ManifestPagesPath is a private directory of authenticated immutable pages.
	ManifestPagesPath string
	TreePath          string
	Backend           string
	// ManifestPath is retained for legacy local tests.
	ManifestPath string
	// ReleaseClaim releases the temporary cache claim after the admission pass.
	ReleaseClaim func()
}

// IntentSource lists the resolved intents this node should serve. The
// cluster wiring supplies it; the broker only sees the revalidated view,
// so its admission rules stay testable without a cluster.
type IntentSource interface {
	ListIntents(ctx context.Context) ([]Intent, error)
}

// ReconcileIntents runs one admission pass: every resolved intent that
// holds no assignment yet is admitted atomically and offered to the
// node's worker, and every offered assignment whose intent is gone or
// changed is drained and retired. Idempotence is keyed by the intent key
// — a tick, a restart, or a repeated list never mints a second
// generation for one revision. A failed listing returns the error and
// changes nothing: losing the source view must never look like the
// intents disappeared.
func ReconcileIntents(ctx context.Context, b *Broker, src IntentSource) error {
	intents, err := src.ListIntents(ctx)
	if err != nil {
		return err
	}
	defer func() {
		for i := range intents {
			if intents[i].ReleaseClaim != nil {
				intents[i].ReleaseClaim()
			}
		}
	}()
	if err := b.RetrySessionTransactions(ctx); err != nil {
		return err
	}
	fresh := make(map[string]bool, len(intents))
	for _, intent := range intents {
		if perr := revalidateIntent(intent); perr == nil {
			fresh[intent.Key] = true
		}
	}
	// An offer whose intent left the resolved view — deleted, changed to
	// another revision, or no longer selecting this node — drains its
	// generation and retires its offer before new intents are admitted, so
	// a superseded pre-native generation releases its quota in the same pass.
	for key := range b.PendingIntentKeys() {
		if fresh[key] {
			continue
		}
		if err := b.RetireIntentAssignment(key); err != nil {
			return err
		}
	}
	if err := b.RetryGenerationReleases(ctx); err != nil {
		return err
	}
	for _, intent := range intents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !fresh[intent.Key] {
			// A malformed intent is skipped, not admitted; the
			// control-plane wiring owns surfacing it where an
			// operator can see it.
			continue
		}
		// AdmitIntentFill is the crash-idempotent admission: the record
		// and its assignment are one durable transaction, and the same
		// revision always answers the same admission.
		if _, perr := b.AdmitIntentFillContext(ctx, intent); perr != nil {
			// Refused admission (exhausted budget, missing grant, unknown
			// artifact) is a per-intent outcome, not a loop failure.
			continue
		}
	}
	return nil
}

// revalidateIntent refuses an intent whose resolved facts cannot back a
// safe reservation: an unverified digest, a zero or negative size, a
// non-positive grant, missing worker and manifest references, an
// incomplete manifest identity, or an unknown backend.
func revalidateIntent(intent Intent) error {
	if !strings.HasPrefix(intent.Digest, "sha256:") ||
		len(intent.Digest) != len("sha256:")+64 {
		return fmt.Errorf("the intent's digest is not a sha256 content address")
	}
	if intent.TotalBytes <= 0 {
		return fmt.Errorf("the intent's resolved size must be positive")
	}
	if intent.Grant.Bytes <= 0 || intent.Grant.Registrations <= 0 || intent.Grant.Sessions <= 0 {
		return fmt.Errorf("the intent's grant must be positive")
	}
	if intent.WorkerID == "" || intent.Namespace == "" {
		return fmt.Errorf("the intent is missing its worker or namespace")
	}
	if intent.ArtifactUID == "" || intent.SourceGeneration < 1 {
		return fmt.Errorf("the intent is missing its artifact UID or source generation")
	}
	if len(intent.ManifestSHA256) != 64 || strings.TrimLeft(intent.ManifestSHA256, "0123456789abcdef") != "" {
		return fmt.Errorf("the intent's manifest digest is not 64 lowercase hex characters")
	}
	if intent.ManifestJSON == "" || intent.TreePath == "" {
		return fmt.Errorf("the intent is missing its manifest bytes or tree path")
	}
	if intent.Backend != "nixl" && intent.Backend != "hixl" {
		return fmt.Errorf("the intent's backend must be nixl or hixl")
	}
	return nil
}

// StartIntents drives the admission pass on a tick until the context
// ends. A list or admission error on one pass is logged by the caller's
// wiring; the loop itself never exits on a per-pass failure.
func StartIntents(ctx context.Context, b *Broker, src IntentSource, tickEvery time.Duration, onErr func(error)) {
	if tickEvery < time.Second {
		tickEvery = time.Second
	}
	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := ReconcileIntents(ctx, b, src); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}
