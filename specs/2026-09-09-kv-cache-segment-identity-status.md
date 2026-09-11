# Spec: KV Cache Segment Identity Status

Status: Shipped
Type: Bug fix

> **Supersedes prior design.** This specification replaces only the member-identity and member-list
> design in these shipped sections; their scale-in conclusion and all unrelated behavior remain
> unchanged:
>
> - `2026-08-28-kv-cache-backend.md`: F6's `status.members[]` shape and listing decoder, F10's
>   segment-identity rationale, and the corresponding T7 and T10 acceptance text.
> - `2026-09-05-kv-cache-media-and-scaling.md`: F5's segment-identity rationale and the
>   shared-identity cases under Unit Tests, Alternatives and Open Questions.
>
> The archived specifications retain local `Corrected after shipping` notes where upstream source
> evidence disproved a factual statement. This specification owns the later serialized API and
> runtime behavior instead of writing that design back into the archived records.

## Summary

Mooncake's segment listing does not make an advertised segment name unique. A member sends its
`local_hostname` as `segment_name`; the randomly bound transfer port appears only in `te_endpoint`.
Two co-located host-network members can therefore report the same portless name while carrying
different `segment_id`, `client_id` and transfer endpoint values.

The previous decoder treated `segment_name` as the unique identity and rejected that valid listing.
The API also keyed `status.members[]` by `segmentName`, so even a decoder that accepted the response
could not store both rows. The controller never reached the attribution rule intended for this
configuration, and tests using port-bearing names hid both defects.

This change decodes the two identifiers the leader already provides, keys status rows by segment ID,
accepts duplicate segment names and keeps Pod attribution separate from segment identity. Rows on a
shared address remain publishable and are credited to no Pod, because no Pod exposes the client ID
that would map a row back to one of those Pods; they still carry the node and the medium their
candidates agree on, neither of which is a fact about the individual Pod.

## Goals

- Accept every well-formed listing whose segment IDs are present and unique, including duplicate
  segment names.
- Publish the leader's segment and client IDs without deriving or rewriting either value.
- Keep every listed segment in status under a Kubernetes list-map key that is actually unique.
- Report ambiguity only for rows on shared addresses; retain determinate node and medium attribution
  elsewhere in the same listing.
- Keep status writable while a development cluster moves from the former schema to this one.
- Preserve the existing scale-in conclusion: no memory-unmount hook is rendered, so removing a member
  still drops its memory segment.

## Non-Goals

- Expose a member's own client ID from inside its Pod. Mooncake does not provide that value through a
  supported member interface.
- Guess which co-located host-network Pod owns a segment. Distinct segment and client IDs identify
  rows, not Pods.
- Add a memory-unmount or data-migration workflow.
- Prove the shared host-network response shape on a live cluster in this change. That validation needs
  a supported host-network transport and is tracked separately.

## Design

### Decode the leader's actual identity fields

`DecodeSegmentListing` reads `segment_id`, `client_id`, `segment_name`, `status`, `protocol` and
`te_endpoint`. Segment and client IDs and the name are required. Segment IDs must be unique because
they become the associative-list key; names are not required to be unique.

The decoder rejects a missing or repeated segment ID rather than accepting an object the API cannot
represent. It rejects a missing client ID because the public status contract promises the leader's
identity tuple on every row. Unknown state strings remain pass-through observations.

### Key status by segment ID

`KVCacheBackendMemberStatus` gains required `segmentID` and `clientID` fields.
`status.members[]` changes its list-map key from `segmentName` to `segmentID`. `segmentName` remains a
required leader observation. Pod attribution joins the host portion of `teEndpoint` to a ready
candidate's address and succeeds only when exactly one candidate matches.

Segment IDs are unique for a mounted listing but are not durable across remounts. They are suitable
for identifying current status rows, not for user-authored references or long-lived identity.

### Separate row identity from Pod attribution

For each decoded segment, the controller always publishes its segment ID, client ID, advertised name,
protocol and state. It credits the segment to a Pod only when the endpoint address maps to exactly
one ready member Pod.

If several ready Pods answer to an address used by one or more segments, no Pod is credited for those
rows and `MembersMounted=False` reports `AmbiguousMemberIdentity`. The message states how many of the
total listed segments are ambiguous. Segments on other addresses in the same response keep their
determinate attribution.

**Corrected after shipping.** This section formerly said those rows retain empty node and medium
fields. Neither field is a fact about the individual Pod — Pods behind one key share a node, and the
medium is their group's declaration — so both are published from what the candidates agree on, and
only a field they disagree on is left empty. Which Pod produced a segment is still unknown and still
reported; the two are decided separately, so a disputed medium does not put the shared node in doubt.

The status list is sorted by segment ID before comparison so leader response ordering does not cause
status churn.

### Development-object compatibility

No version tag contains the former KVCacheBackend CRD, so there is no released object compatibility
surface. Development clusters can still have stored member rows that predate `segmentID` and
`clientID`. Once the CRD requires those fields and uses `segmentID` as the list key, carrying an old
row into a later status update can fail validation on API servers that do not ratchet unchanged CRD
validation failures.

The controller does not synthesize an ID from `segmentName`: an address is not an ID and may be
duplicated. It also does not retain only the rows that happen to be valid, because a partial stale
listing would claim that other mounted segments disappeared. Before every status write, if any
retained row lacks either new identity, the controller omits the whole legacy listing and sets
`MembersMounted=False`. The condition explains that the prior rows predate the required identities
and will be replaced by the next successful leader read. A controller-owned `LegacyMemberStatus`
reason persists that explanation across repeated read failures; external response text is never used
as a state marker. The pool controller applies the same cleanup before changing the backend's
`usedBy`, so its independent status writer cannot carry an old row around the guard or leave the
backend `Ready` after omitting its member list. A successful read replaces the legacy list and its
condition normally.

This is a one-way development migration. No conversion webhook or permanent compatibility field is
added for an API that has not appeared in a release.

## Verification

Unit tests use the source-defined wire shape: portless names, distinct segment and client IDs and the
random transfer port only in `te_endpoint`. They cover:

- duplicate names with unique IDs decode successfully;
- missing identities and duplicate segment IDs are rejected;
- two shared-host rows are both published and neither is attributed to a Pod;
- a mixed listing keeps the uniquely addressable row's node and medium while scoping the condition to
  the ambiguous rows;
- reversing leader response order produces no status write;
- a retained pre-change row is omitted on failed listing reads, the condition explanation persists,
  and a successful read clears the migration state;
- external response text cannot impersonate the controller-owned migration state;
- a pool claim update cannot preserve a retained pre-change row through its separate status writer;
- generated CRD schema and apply-configuration mirrors require both fields and key members by
  `segmentID`.

The separate live validation must construct two member groups co-located on one node under a
supported host-network transport and capture `/get_segments_detail`. It must prove that the names are
identical and portless, the IDs and transfer endpoints are distinct, both status rows persist, and
the ambiguity condition credits neither row to a Pod while both carry the node and medium their
candidates agree on. Moving the groups to different nodes must make both rows attributable; a
same-node TCP case must remain a non-ambiguous control.
