package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"

	_ "embed"
)

// The identity seam this observer reads is frozen: the Ray node label that carries the
// owning Pod's UID. The workload render passes it to `ray start --labels`, Ray persists
// it in GcsNodeInfo.labels, and every join this file performs goes through it. A Pod-IP
// or Pod-name join would survive a same-name replacement and is deliberately not used.
const modelDeploymentElasticPodUIDLabelKey = "gpustack-pod-uid"

//go:embed model_deployment_elastic_observer.py
var modelDeploymentElasticObserverScript string

// Bounds on what one observation may claim. They are parser bounds, not transport
// bounds: the exec transport enforces its own byte cap, and these keep a malformed but
// well-formed-looking document from being walked unboundedly.
const (
	modelDeploymentElasticMaxNodes  = 4096
	modelDeploymentElasticMaxActors = 4096
	modelDeploymentElasticMaxPGs    = 4096

	// Total bound for one Observe call. An earlier caller deadline still wins: the
	// budget is installed with context.WithTimeout, so whichever expires first fires.
	modelDeploymentElasticObservationBudget = 45 * time.Second

	// Per-exec bound handed to the script so its own writer stops before the transport's
	// cap and the refusal (exit 4) is what arrives instead of a truncated stream.
	modelDeploymentElasticScriptMaxOutputBytes = 6 << 20

	modelDeploymentElasticScriptDeadlineSeconds = 20.0

	// The GCS address the head's own control plane listens on by default. The caller
	// supplies the address from the rendered head spec; an empty capture is refused.
	modelDeploymentElasticDefaultGCSAddress = "127.0.0.1:6379"
)

// Native DP actor classes. A rank proof requires an actor of exactly one of these
// classes; every other alive actor on a member still counts against that member's
// actor-free fact.
const (
	modelDeploymentElasticActorClassEngineCore  = "EngineCoreActor"
	modelDeploymentElasticActorClassDPMoEEngine = "DPMoEEngineCoreActor"
)

// Native placement-group names are dp_rank_N, produced by the pinned vLLM engine at
// v1/engine/utils.py:739 (initial) and :843 (scale-up).
const modelDeploymentElasticRankPGPrefix = "dp_rank_"

// Raw GCS enum values the parser accepts. Anything outside them is Unknown, never a
// default: an unrecognized state is a schema drift this file refuses to interpret.
const (
	elasticRayActorDependenciesUnready = 0
	elasticRayActorPendingCreation     = 1
	elasticRayActorAlive               = 2
	elasticRayActorRestarting          = 3
	elasticRayActorDead                = 4

	elasticRayPGPending      = 0
	elasticRayPGPrepared     = 1
	elasticRayPGCreated      = 2
	elasticRayPGRemoved      = 3
	elasticRayPGRescheduling = 4
)

// modelDeploymentElasticMemberRole is the captured role of one owned Pod.
type modelDeploymentElasticMemberRole string

const (
	modelDeploymentElasticRoleHead   modelDeploymentElasticMemberRole = "head"
	modelDeploymentElasticRoleMaster modelDeploymentElasticMemberRole = "master"
	modelDeploymentElasticRoleWorker modelDeploymentElasticMemberRole = "worker"
)

// modelDeploymentElasticCapturedMember is one captured owned Pod: name, UID, container and
// role as the controller recorded them before observing. Every runtime fact this file
// produces is bound back to these captures by uncached reads.
type modelDeploymentElasticCapturedMember struct {
	PodUID    types.UID
	PodName   string
	Container string
	Role      modelDeploymentElasticMemberRole
}

// Wire shapes. Required fields are pointers so an absent or null field is refused
// instead of read as a zero: a zero here is a claim that nothing exists, and the
// protocol acts on zeros.
type elasticRayNodeWire struct {
	NodeIDHex           *string           `json:"node_id_hex"`
	Alive               *bool             `json:"alive"`
	NodeManagerAddress  string            `json:"node_manager_address"`
	NodeManagerHostname string            `json:"node_manager_hostname"`
	NodeManagerPort     *int              `json:"node_manager_port"`
	NodeName            string            `json:"node_name"`
	DeathReason         *int              `json:"death_reason"`
	DeathReasonMessage  string            `json:"death_reason_message"`
	Labels              map[string]string `json:"labels"`
	Resources           map[string]string `json:"resources"`
}

type elasticRayActorWire struct {
	ActorIDHex          *string `json:"actor_id_hex"`
	State               *int    `json:"state"`
	StateName           string  `json:"state_name"`
	ClassName           string  `json:"class_name"`
	NodeIDHex           string  `json:"node_id_hex"`
	PlacementGroupIDHex string  `json:"placement_group_id_hex"`
}

type elasticRayBundleWire struct {
	BundleIndex *int   `json:"bundle_index"`
	NodeIDHex   string `json:"node_id_hex"`
}

type elasticRayPGWire struct {
	PlacementGroupIDHex *string                `json:"placement_group_id_hex"`
	Name                *string                `json:"name"`
	State               *int                   `json:"state"`
	StateName           string                 `json:"state_name"`
	Bundles             []elasticRayBundleWire `json:"bundles"`
}

type elasticRayErrorWire struct {
	Table string `json:"table"`
	Error string `json:"error"`
}

type elasticRayDocumentWire struct {
	SchemaVersion   *int                   `json:"schema_version"`
	RayVersion      *string                `json:"ray_version"`
	GCSAddress      string                 `json:"gcs_address"`
	CollectedAtMS   *int64                 `json:"collected_at_ms"`
	Nodes           *[]elasticRayNodeWire  `json:"nodes"`
	Actors          *[]elasticRayActorWire `json:"actors"`
	PlacementGroups *[]elasticRayPGWire    `json:"placement_groups"`
	Errors          []elasticRayErrorWire  `json:"errors"`
}

// rejectDuplicateJSONKeys walks the token stream of one JSON document and refuses the
// first object that states one key twice. encoding/json itself silently keeps the last
// duplicate, so a payload that carries `{"state":2,"state":4}` would otherwise parse as
// whichever value the decoder saw last.
func rejectDuplicateJSONKeys(dec *json.Decoder) error {
	return walkDuplicateJSONKeys(dec, 0)
}

func walkDuplicateJSONKeys(dec *json.Decoder, depth int) error {
	if depth > 64 {
		return fmt.Errorf("json nesting exceeds 64")
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if seen[key] {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = true
			if err := walkDuplicateJSONKeys(dec, depth+1); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	case '[':
		for dec.More() {
			if err := walkDuplicateJSONKeys(dec, depth+1); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	default:
		return nil
	}
}

// parseElasticRayDocument decodes one collector document strictly: no unknown fields,
// no duplicate keys, no trailing value, every required table and field present, and
// table sizes inside the parser bounds. A document that fails any of these is an error,
// which the caller turns into Unknown rather than into an empty cluster.
func parseElasticRayDocument(data []byte) (*elasticRayDocumentWire, error) {
	scanner := json.NewDecoder(strings.NewReader(string(data)))
	if err := rejectDuplicateJSONKeys(scanner); err != nil {
		return nil, fmt.Errorf("collector document is not strict JSON: %w", err)
	}

	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var doc elasticRayDocumentWire
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("collector document is not an observation: %w", err)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, errors.New("collector document carries a trailing JSON value")
	}

	if doc.SchemaVersion == nil || *doc.SchemaVersion != 1 {
		return nil, errors.New("collector document carries no schema_version 1")
	}
	if doc.RayVersion == nil || *doc.RayVersion == "" {
		return nil, errors.New("collector document carries no ray_version")
	}
	if doc.CollectedAtMS == nil {
		return nil, errors.New("collector document carries no collected_at_ms")
	}
	if doc.Nodes == nil || doc.Actors == nil || doc.PlacementGroups == nil {
		return nil, errors.New("collector document is missing a required table")
	}
	if len(doc.Errors) > 0 {
		return nil, fmt.Errorf("collector refused %d table reads", len(doc.Errors))
	}
	if len(*doc.Nodes) > modelDeploymentElasticMaxNodes {
		return nil, fmt.Errorf("collector document carries %d nodes", len(*doc.Nodes))
	}
	if len(*doc.Actors) > modelDeploymentElasticMaxActors {
		return nil, fmt.Errorf("collector document carries %d actors", len(*doc.Actors))
	}
	if len(*doc.PlacementGroups) > modelDeploymentElasticMaxPGs {
		return nil, fmt.Errorf("collector document carries %d placement groups", len(*doc.PlacementGroups))
	}

	for i := range *doc.Nodes {
		if err := elasticRayNodeRequired(&(*doc.Nodes)[i]); err != nil {
			return nil, fmt.Errorf("node %d: %w", i, err)
		}
	}
	for i := range *doc.Actors {
		if err := elasticRayActorRequired(&(*doc.Actors)[i]); err != nil {
			return nil, fmt.Errorf("actor %d: %w", i, err)
		}
	}
	for i := range *doc.PlacementGroups {
		if err := elasticRayPGRequired(&(*doc.PlacementGroups)[i]); err != nil {
			return nil, fmt.Errorf("placement group %d: %w", i, err)
		}
	}
	return &doc, nil
}

func elasticRayNodeRequired(node *elasticRayNodeWire) error {
	if node.NodeIDHex == nil || *node.NodeIDHex == "" {
		return errors.New("carries no node_id_hex")
	}
	if node.Alive == nil {
		return errors.New("carries no alive")
	}
	if node.Labels == nil {
		return errors.New("carries no labels table")
	}
	return nil
}

func elasticRayActorRequired(actor *elasticRayActorWire) error {
	if actor.ActorIDHex == nil || *actor.ActorIDHex == "" {
		return errors.New("carries no actor_id_hex")
	}
	if actor.State == nil {
		return errors.New("carries no state")
	}
	switch *actor.State {
	case elasticRayActorDependenciesUnready,
		elasticRayActorPendingCreation,
		elasticRayActorAlive,
		elasticRayActorRestarting,
		elasticRayActorDead:
	default:
		return fmt.Errorf("unknown actor state %d", *actor.State)
	}
	return nil
}

func elasticRayPGRequired(pg *elasticRayPGWire) error {
	if pg.PlacementGroupIDHex == nil || *pg.PlacementGroupIDHex == "" {
		return errors.New("carries no placement_group_id_hex")
	}
	if pg.Name == nil {
		return errors.New("carries no name")
	}
	if pg.State == nil {
		return errors.New("carries no state")
	}
	switch *pg.State {
	case elasticRayPGPending, elasticRayPGPrepared, elasticRayPGCreated,
		elasticRayPGRemoved, elasticRayPGRescheduling:
	default:
		return fmt.Errorf("unknown placement group state %d", *pg.State)
	}
	return nil
}

// modelDeploymentElasticActorFreeFact is the per-captured-UID actor-free answer: the
// bool is only meaningful with its reason, and a false never deletes anything.
type modelDeploymentElasticActorFreeFact struct {
	ActorFree bool
	Reason    string
}

// modelDeploymentElasticObservation feeds the resize controller: separately known width, the
// rank to Pod UID map, complete actor identities, and per-captured-UID actor-free facts
// with explicit reasons. It never authorizes a resize, a deletion, or a completion.
type modelDeploymentElasticObservation struct {
	SchemaVersion int
	RayVersion    string
	GCSAddress    string
	CollectedAtMS int64

	WidthKnown         bool
	RayWidth           int
	WidthUnknownReason string
	RankToPodUID       map[int]types.UID

	// RegisteredGPU is the NODE side of the four-layer join: how many captured GPU
	// members currently sit, one whole GPU each, on unique ALIVE Ray nodes. It is
	// deliberately independent of RayWidth — a scale-up registers capacity before the
	// native ranks exist — and a known zero means genuinely no GPU members are captured.
	RegisteredGPUKnown         bool
	RegisteredGPU              int
	RegisteredGPUUnknownReason string

	ActorsComplete bool

	ActorFree map[types.UID]modelDeploymentElasticActorFreeFact

	UnknownReasons []string
}

// isNativeElasticActorClass reports whether a GCS actor class is one of the native DP
// engine actor classes a rank proof may be satisfied by.
func isNativeElasticActorClass(class string) bool {
	return class == modelDeploymentElasticActorClassEngineCore ||
		class == modelDeploymentElasticActorClassDPMoEEngine
}

// elasticRayRankOfName parses dp_rank_N strictly: the exact prefix and nothing after
// the integer. A name that only prefixes the token, or carries a suffix, is not a rank.
func elasticRayRankOfName(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, modelDeploymentElasticRankPGPrefix)
	if !ok || rest == "" {
		return 0, false
	}
	rank := 0
	for _, r := range rest {
		if r < '0' || r > '9' {
			return 0, false
		}
		rank = rank*10 + int(r-'0')
		if rank > 1<<20 {
			return 0, false
		}
	}
	return rank, true
}

// joinModelDeploymentElasticRayIdentity binds one collector document to the captured
// members. Every rule below is a hold, not a zero: anything the document does not prove
// is carried as an explicit Unknown reason, because a missing proof is what retirement
// safety is made of.
func joinModelDeploymentElasticRayIdentity(
	doc *elasticRayDocumentWire,
	members []modelDeploymentElasticCapturedMember,
	expectedWidth int32,
) modelDeploymentElasticObservation {
	obs := modelDeploymentElasticObservation{
		SchemaVersion:  *doc.SchemaVersion,
		RayVersion:     *doc.RayVersion,
		GCSAddress:     doc.GCSAddress,
		CollectedAtMS:  *doc.CollectedAtMS,
		WidthKnown:     true,
		RankToPodUID:   map[int]types.UID{},
		ActorFree:      map[types.UID]modelDeploymentElasticActorFreeFact{},
		UnknownReasons: []string{},
	}

	memberByUID := map[types.UID]modelDeploymentElasticCapturedMember{}
	for _, member := range members {
		if _, dup := memberByUID[member.PodUID]; dup {
			obs.WidthKnown = false
			obs.WidthUnknownReason = "captured members repeat Pod UID " + string(member.PodUID)
			return obs
		}
		memberByUID[member.PodUID] = member
	}

	// Node joins. Only ALIVE nodes make claims: a DEAD row is history — a restarted
	// Pod's old node, a retired foreign box — and must neither count toward registered
	// membership nor duplicate the ALIVE node that now carries the same Pod UID. DEAD
	// rows stay in the raw lookup so actors sitting on them are still resolved.
	nodeByHex := map[string]*elasticRayNodeWire{}
	nodeHexByUID := map[types.UID]string{}
	headHexByUID := map[types.UID]string{}
	duplicateClaimUIDs := map[types.UID]bool{}
	for i := range *doc.Nodes {
		node := &(*doc.Nodes)[i]
		if _, dup := nodeByHex[*node.NodeIDHex]; dup {
			obs.addUnknown(fmt.Sprintf("nodes repeat node id %s", *node.NodeIDHex))
			continue
		}
		nodeByHex[*node.NodeIDHex] = node
		if !*node.Alive {
			continue
		}
		uid, claimed := node.Labels[modelDeploymentElasticPodUIDLabelKey]
		if !claimed {
			continue
		}
		member, known := memberByUID[types.UID(uid)]
		if !known {
			obs.addUnknown(fmt.Sprintf("node %s claims foreign Pod UID %s", *node.NodeIDHex, uid))
			continue
		}
		if member.Role == modelDeploymentElasticRoleHead {
			if _, dup := headHexByUID[member.PodUID]; dup {
				obs.addUnknown(fmt.Sprintf("head Pod UID %s is claimed by two Ray nodes", uid))
				continue
			}
			headHexByUID[member.PodUID] = *node.NodeIDHex
			continue
		}
		if _, dup := nodeHexByUID[member.PodUID]; dup {
			duplicateClaimUIDs[member.PodUID] = true
			obs.addUnknown(fmt.Sprintf("Pod UID %s is claimed by two ALIVE Ray nodes (replacement)", uid))
			continue
		}
		nodeHexByUID[member.PodUID] = *node.NodeIDHex
	}
	for _, member := range members {
		if member.Role == modelDeploymentElasticRoleHead {
			continue
		}
		if _, joined := nodeHexByUID[member.PodUID]; !joined {
			obs.addUnknown(fmt.Sprintf(
				"captured %s Pod %s (uid %s) is claimed by no live Ray node",
				member.Role, member.PodName, member.PodUID))
		}
	}

	// Registered GPU capacity is the NODE side of the four-layer join, independent of the
	// native rank side: how many captured GPU members an ALIVE Ray node currently carries
	// one whole GPU for. A scale-up registers capacity before the engine creates the new
	// ranks, so this must be known on its own or the kernel deadlocks waiting for ranks
	// that the command has not created yet. Zero captured GPU members is a known zero;
	// a captured GPU member with no current ALIVE node is Unknown, never a short count.
	obs.RegisteredGPUKnown = true
	gpuMembers := 0
	for _, member := range members {
		if member.Role == modelDeploymentElasticRoleHead {
			continue
		}
		gpuMembers++
		nodeHex, joined := nodeHexByUID[member.PodUID]
		if !joined || duplicateClaimUIDs[member.PodUID] {
			obs.RegisteredGPUKnown = false
			obs.RegisteredGPUUnknownReason = fmt.Sprintf(
				"registered GPU capacity unknown: captured %s Pod %s has no unique current ALIVE Ray node",
				member.Role, member.PodName)
			continue
		}
		rawGPU, carried := nodeByHex[nodeHex].Resources["GPU"]
		value, err := strconv.ParseFloat(rawGPU, 64)
		switch {
		case !carried:
			obs.RegisteredGPUKnown = false
			obs.RegisteredGPUUnknownReason = fmt.Sprintf(
				"registered GPU capacity unknown: node %s states no GPU resource", nodeHex)
		case err != nil:
			obs.RegisteredGPUKnown = false
			obs.RegisteredGPUUnknownReason = fmt.Sprintf(
				"registered GPU capacity unknown: node %s GPU resource %q is not a number", nodeHex, rawGPU)
		case math.IsInf(value, 0) || math.IsNaN(value):
			obs.RegisteredGPUKnown = false
			obs.RegisteredGPUUnknownReason = fmt.Sprintf(
				"registered GPU capacity unknown: node %s GPU resource is not finite", nodeHex)
		case value != 1:
			obs.RegisteredGPUKnown = false
			obs.RegisteredGPUUnknownReason = fmt.Sprintf(
				"registered GPU capacity unknown: node %s carries %v GPUs, not exactly one", nodeHex, value)
		default:
			obs.RegisteredGPU++
		}
	}
	if gpuMembers == 0 {
		obs.RegisteredGPU = 0
	}

	// Actor inventory. Duplicates are schema drift; unknown-state rows were refused at
	// parse. Every actor's node must resolve, or complete actor identities is false.
	actorByID := map[string]*elasticRayActorWire{}
	actorsByNode := map[string][]*elasticRayActorWire{}
	actorsByPG := map[string][]*elasticRayActorWire{}
	actorsComplete := true
	for i := range *doc.Actors {
		actor := &(*doc.Actors)[i]
		if _, dup := actorByID[*actor.ActorIDHex]; dup {
			obs.addUnknown(fmt.Sprintf("actors repeat actor id %s", *actor.ActorIDHex))
			actorsComplete = false
			continue
		}
		actorByID[*actor.ActorIDHex] = actor
		actorsByNode[actor.NodeIDHex] = append(actorsByNode[actor.NodeIDHex], actor)
		actorsByPG[actor.PlacementGroupIDHex] = append(actorsByPG[actor.PlacementGroupIDHex], actor)
		if _, known := nodeByHex[actor.NodeIDHex]; !known {
			obs.addUnknown(fmt.Sprintf(
				"actor %s (%s) sits on unknown node %s",
				*actor.ActorIDHex, actor.ClassName, actor.NodeIDHex))
			actorsComplete = false
			continue
		}
		// A nonterminal actor parked on a DEAD node is an unresolved live worker the
		// cluster has not accounted for; DEAD actors on DEAD nodes are history.
		if node := nodeByHex[actor.NodeIDHex]; !*node.Alive && *actor.State != elasticRayActorDead {
			obs.addUnknown(fmt.Sprintf(
				"nonterminal actor %s (%s) in state %s sits on DEAD node %s",
				*actor.ActorIDHex, actor.ClassName, actor.StateName, actor.NodeIDHex))
			actorsComplete = false
		}
	}
	obs.ActorsComplete = actorsComplete

	// Rank proofs. Each expected rank needs its dp_rank_N placement group CREATED, one
	// ALIVE native actor scheduled on it, and that actor's node alive and joined to the
	// captured GPU member. Pending, restarting, removed and rescheduled states hold.
	pgByRank := map[int]*elasticRayPGWire{}
	for i := range *doc.PlacementGroups {
		pg := &(*doc.PlacementGroups)[i]
		rank, isRank := elasticRayRankOfName(*pg.Name)
		if !isRank {
			continue
		}
		if *pg.State == elasticRayPGRemoved {
			for _, actor := range actorsByPG[*pg.PlacementGroupIDHex] {
				if *actor.State != elasticRayActorDead {
					obs.addUnknown(fmt.Sprintf("nonterminal actor %s remains on removed rank %d", *actor.ActorIDHex, rank))
					obs.ActorsComplete = false
				}
			}
			continue
		}
		if _, dup := pgByRank[rank]; dup {
			obs.addUnknown(fmt.Sprintf("placement groups repeat rank %d (name %s)", rank, *pg.Name))
			continue
		}
		pgByRank[rank] = pg
	}

	// addWidthReason is addUnknown: a rank proof that does not close is an unknown the
	// reason string names, not a smaller width.
	addWidthReason := obs.addUnknown

	for rank := 0; rank < int(expectedWidth); rank++ {
		pg, known := pgByRank[rank]
		if !known {
			addWidthReason(fmt.Sprintf("missing native rank %d placement group", rank))
			continue
		}
		if *pg.State != elasticRayPGCreated {
			addWidthReason(fmt.Sprintf(
				"rank %d placement group is %s, not CREATED", rank, pg.StateName))
			continue
		}
		alive := 0
		proofUID := types.UID("")
		for _, actor := range actorsByPG[*pg.PlacementGroupIDHex] {
			if *actor.State == elasticRayActorDead {
				// A DEAD actor on the rank group is restart history, not a rank claim.
				continue
			}
			if *actor.State != elasticRayActorAlive {
				addWidthReason(fmt.Sprintf(
					"rank %d carries nonterminal native actor %s in state %s",
					rank, *actor.ActorIDHex, actor.StateName))
				continue
			}
			if !isNativeElasticActorClass(actor.ClassName) {
				continue
			}
			node, known := nodeByHex[actor.NodeIDHex]
			if !known || node.Alive == nil || !*node.Alive {
				addWidthReason(fmt.Sprintf(
					"rank %d native actor %s is not at an alive Ray node", rank, *actor.ActorIDHex))
				continue
			}
			uid, claimed := node.Labels[modelDeploymentElasticPodUIDLabelKey]
			member, knownMember := memberByUID[types.UID(uid)]
			if !claimed || !knownMember || member.Role == modelDeploymentElasticRoleHead {
				addWidthReason(fmt.Sprintf(
					"rank %d native actor %s is not at a captured GPU member", rank, *actor.ActorIDHex))
				continue
			}
			alive++
			proofUID = member.PodUID
		}
		if alive != 1 || proofUID == "" {
			addWidthReason(fmt.Sprintf("rank %d has no single alive native actor at a captured GPU member", rank))
			continue
		}
		obs.RankToPodUID[rank] = proofUID
	}
	for rank, pg := range pgByRank {
		// A REMOVED rank beyond the expected width is drain residue from the resize this
		// observation follows; it is history, not an emerging width. Any other state
		// beyond the expected width is a native rank the engine still holds and holds.
		if rank >= int(expectedWidth) && *pg.State != elasticRayPGRemoved {
			addWidthReason(fmt.Sprintf(
				"unexpected native rank %d placement group is %s", rank, pg.StateName))
		}
	}

	obs.RayWidth = len(obs.RankToPodUID)

	// Actor-free facts, per captured GPU member. ALL actors on the member's node count,
	// of every class: a non-native leftover is exactly what a drain must not leave
	// behind. DEAD actors are history; every nonterminal state blocks. The fact requires
	// the captured UID's UNIQUE current ALIVE node — a duplicated claim is not a node
	// this fact may be read from.
	for _, member := range members {
		if member.Role == modelDeploymentElasticRoleHead {
			continue
		}
		nodeHex, joined := nodeHexByUID[member.PodUID]
		if !joined || duplicateClaimUIDs[member.PodUID] {
			obs.ActorFree[member.PodUID] = modelDeploymentElasticActorFreeFact{
				ActorFree: false,
				Reason: fmt.Sprintf(
					"no unique current ALIVE Ray node claims Pod %s (uid %s)", member.PodName, member.PodUID),
			}
			continue
		}
		blocking := 0
		reason := ""
		for _, actor := range actorsByNode[nodeHex] {
			if *actor.State == elasticRayActorDead {
				continue
			}
			blocking++
			if reason == "" {
				reason = fmt.Sprintf("actor %s (%s) in state %s",
					*actor.ActorIDHex, actor.ClassName, actor.StateName)
			}
		}
		if blocking > 0 {
			obs.ActorFree[member.PodUID] = modelDeploymentElasticActorFreeFact{
				ActorFree: false,
				Reason: fmt.Sprintf(
					"%d nonterminal actor(s) remain on Pod %s: %s",
					blocking, member.PodName, reason),
			}
			continue
		}
		obs.ActorFree[member.PodUID] = modelDeploymentElasticActorFreeFact{ActorFree: true}
	}
	return obs
}

func (o *modelDeploymentElasticObservation) addUnknown(reason string) {
	// Every unknown is also a width hold: an observation that cannot prove a structural
	// fact cannot prove a width either, and a width proven beside a structural unknown
	// would be the false zero this file exists to refuse.
	o.UnknownReasons = append(o.UnknownReasons, reason)
	o.WidthKnown = false
	if o.WidthUnknownReason != "" {
		o.WidthUnknownReason += "; "
	}
	o.WidthUnknownReason += reason
}

// unknownElasticObservation is the shape every Observe hold returns: nothing proven,
// every seam's Unknown reason carrying the same cause.
func unknownElasticObservation(reason string) modelDeploymentElasticObservation {
	return modelDeploymentElasticObservation{
		UnknownReasons:             []string{reason},
		WidthUnknownReason:         reason,
		RegisteredGPUUnknownReason: reason,
		RankToPodUID:               map[int]types.UID{},
		ActorFree:                  map[types.UID]modelDeploymentElasticActorFreeFact{},
	}
}

// modelDeploymentElasticObserver reads one elastic deployment's Ray identity through
// the existing exec transport. It adds no second exec implementation: the drain
// collector's bounded SPDY stream is the transport, and this observer owns only the
// script, the K8s identity checks around it, and the strict parse and join.
type modelDeploymentElasticObserver struct {
	client ctrlcli.Client
	fresh  ctrlcli.Reader
	exec   modelDeploymentDrainExec
	budget time.Duration
}

// newModelDeploymentElasticObserver builds the production observer on the same
// transport the drain collector uses. A nil exec installs the drain collector's real
// one; a test may replace it through the same field.
func newModelDeploymentElasticObserver(
	client ctrlcli.Client, fresh ctrlcli.Reader, restConfig *rest.Config,
	core corev1client.CoreV1Interface,
) *modelDeploymentElasticObserver {
	drain := newModelDeploymentDrainCollector(client, fresh, restConfig, core)
	return &modelDeploymentElasticObserver{
		client: client,
		fresh:  fresh,
		exec:   drain.exec,
		budget: modelDeploymentElasticObservationBudget,
	}
}

// modelDeploymentElasticIdentity is the before/after identity a read is bound to: the
// live deployment's UID and generation must not move between the two uncached reads.
type modelDeploymentElasticIdentity struct {
	UID        types.UID
	Generation int64
}

// Observe collects and joins one elastic observation. It fails closed: a member that
// moved, a container that is not running, a document that does not parse, or a
// deployment whose identity drifted between the bounding reads is Unknown with the
// reason named, never an empty cluster.
func (o *modelDeploymentElasticObserver) Observe(
	ctx context.Context,
	md *workercore.ModelDeployment,
	members []modelDeploymentElasticCapturedMember,
	expectedWidth int32,
	gcsAddress string,
) (modelDeploymentElasticObservation, error) {
	if o.exec == nil {
		return modelDeploymentElasticObservation{}, errors.New("no exec transport is configured")
	}
	if gcsAddress == "" {
		gcsAddress = modelDeploymentElasticDefaultGCSAddress
	}

	ctx, cancel := context.WithTimeout(ctx, o.budget)
	defer cancel()

	before, drift := o.liveIdentity(ctx, md, nil)
	if drift != "" {
		return unknownElasticObservation(drift), nil
	}

	// Every captured member is bound to a live Pod before the exec: same name, same
	// UID, owned by this deployment, engine container running. The read then runs in
	// the captured CPU head's container — the head proves its own captured identity
	// the same way, but is never a GPU width member.
	var head *corev1.Pod
	for _, member := range members {
		pod, drift := o.liveMember(ctx, md, &before, member)
		if drift != "" {
			return unknownElasticObservation(drift), nil
		}
		if member.Role == modelDeploymentElasticRoleHead {
			head = pod
		}
	}
	if head == nil {
		reason := "captured members name no CPU head to read through"
		return unknownElasticObservation(reason), nil
	}

	argv := []string{
		"python3", "-c", modelDeploymentElasticObserverScript,
		"--gcs", gcsAddress,
		"--deadline-seconds", fmt.Sprintf("%g", modelDeploymentElasticScriptDeadlineSeconds),
		"--max-output-bytes", fmt.Sprintf("%d", modelDeploymentElasticScriptMaxOutputBytes),
	}
	body, err := o.exec(ctx, head, headContainerOf(members), argv)
	if err != nil {
		reason := fmt.Sprintf("ray identity exec failed: %v", err)
		return unknownElasticObservation(reason), nil
	}

	doc, err := parseElasticRayDocument([]byte(body))
	if err != nil {
		reason := fmt.Sprintf("ray identity document refused: %v", err)
		return unknownElasticObservation(reason), nil
	}

	after, drift := o.liveIdentity(ctx, md, &before)
	if drift != "" {
		return unknownElasticObservation(drift), nil
	}
	for _, member := range members {
		if _, drift := o.liveMember(ctx, md, &after, member); drift != "" {
			return unknownElasticObservation(drift), nil
		}
	}

	return joinModelDeploymentElasticRayIdentity(doc, members, expectedWidth), nil
}

// liveIdentity reads the deployment uncached and, when expected is non-nil, refuses a
// UID or generation that moved under it.
func (o *modelDeploymentElasticObserver) liveIdentity(
	ctx context.Context, md *workercore.ModelDeployment, expected *modelDeploymentElasticIdentity,
) (modelDeploymentElasticIdentity, string) {
	live := &workercore.ModelDeployment{}
	if err := o.fresh.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: md.Name}, live); err != nil {
		return modelDeploymentElasticIdentity{}, fmt.Sprintf("live deployment read failed: %v", err)
	}
	current := modelDeploymentElasticIdentity{UID: live.UID, Generation: live.Generation}
	if expected != nil && (current.UID != expected.UID || current.Generation != expected.Generation) {
		return modelDeploymentElasticIdentity{}, fmt.Sprintf(
			"deployment identity drifted during observation (uid %s->%s generation %d->%d)",
			expected.UID, current.UID, expected.Generation, current.Generation)
	}
	return current, ""
}

// liveMember binds one captured member to its live Pod: same name, same UID, owned by
// the deployment captured above, container present and running.
func (o *modelDeploymentElasticObserver) liveMember(
	ctx context.Context, md *workercore.ModelDeployment,
	deployment *modelDeploymentElasticIdentity, member modelDeploymentElasticCapturedMember,
) (*corev1.Pod, string) {
	pod := &corev1.Pod{}
	if err := o.fresh.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: member.PodName}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Sprintf("captured Pod %s is gone", member.PodName)
		}
		return nil, fmt.Sprintf("live Pod %s read failed: %v", member.PodName, err)
	}
	if pod.UID != member.PodUID {
		return nil, fmt.Sprintf(
			"Pod %s carries uid %s, captured uid is %s (same-name replacement)",
			member.PodName, pod.UID, member.PodUID)
	}
	owned := false
	for _, ref := range pod.OwnerReferences {
		if ref.UID == deployment.UID {
			owned = true
			break
		}
	}
	if !owned {
		return nil, fmt.Sprintf("Pod %s is not owned by deployment uid %s", member.PodName, deployment.UID)
	}
	running := false
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == member.Container && status.State.Running != nil {
			running = true
			break
		}
	}
	if !running {
		return nil, fmt.Sprintf("Pod %s container %s is not running", member.PodName, member.Container)
	}
	return pod, ""
}

// headContainerOf picks the container the exec runs in: the captured CPU head's own
// container. The head is where the Ray control plane and its GCS listen.
func headContainerOf(members []modelDeploymentElasticCapturedMember) string {
	for _, member := range members {
		if member.Role == modelDeploymentElasticRoleHead {
			return member.Container
		}
	}
	return ""
}
