"""The loading group: one agreed decision across every rank.

A loading group is the set of engine ranks whose weight-loading
collective sequence must agree — distinct from independent consumer
replicas. ModelExpress strategies are chosen per process; if two ranks
of one group disagree about backend availability or the chosen strategy,
their collective sequence cannot agree, and the group refuses before any
rank mutates a parameter.

The coordinator is deliberately transport-free: it records what each rank
proposed and decides agreement, nothing more. The Rust server is not
involved — GPUStack source identity and the verified host-memory service
stand on their own.
"""

from dataclasses import dataclass, field


class GroupDisagreementError(Exception):
    """The ranks of one loading group did not agree."""


@dataclass(frozen=True)
class RankProposal:
    """One rank's answer: what it sees, and what it would run."""

    rank: int
    backend_available: bool
    chosen_strategy: str


@dataclass
class LoadGroupCoordinator:
    """Collect the group's proposals and decide agreement.

    agree() succeeds only when every rank proposes the same backend
    availability and the same strategy; a rank that never proposed is a
    disagreement the group can name, not a silence waved through.
    """

    expected_size: int = 1
    proposals: list = field(default_factory=list)

    def __post_init__(self):
        if self.expected_size < 1:
            raise ValueError(
                "a loading group has at least one rank; "
                f"expected_size={self.expected_size}"
            )

    def propose(self, proposal: RankProposal) -> None:
        if not 0 <= proposal.rank < self.expected_size:
            raise GroupDisagreementError(
                f"rank {proposal.rank} is outside the loading group "
                f"(ranks 0..{self.expected_size - 1})"
            )
        if any(p.rank == proposal.rank for p in self.proposals):
            raise GroupDisagreementError(f"rank {proposal.rank} proposed twice")
        self.proposals.append(proposal)

    def agree(self) -> RankProposal:
        ranks = {p.rank for p in self.proposals}
        expected = set(range(self.expected_size))
        if ranks != expected:
            missing = sorted(expected - ranks)
            unexpected = sorted(ranks - expected)
            detail = f"the group expected ranks {sorted(expected)} and received {sorted(ranks)}"
            if missing:
                detail += f"; missing ranks {missing}"
            if unexpected:
                detail += f"; unexpected ranks {unexpected}"
            raise GroupDisagreementError(detail)
        first = self.proposals[0]
        for other in self.proposals[1:]:
            if other.backend_available != first.backend_available:
                raise GroupDisagreementError(
                    f"ranks disagree on backend availability: rank "
                    f"{first.rank} says {first.backend_available}, rank "
                    f"{other.rank} says {other.backend_available}"
                )
            if other.chosen_strategy != first.chosen_strategy:
                raise GroupDisagreementError(
                    f"ranks disagree on the chosen strategy: rank "
                    f"{first.rank} chose {first.chosen_strategy!r}, rank "
                    f"{other.rank} chose {other.chosen_strategy!r}"
                )
        return first


__all__ = [
    "GroupDisagreementError",
    "LoadGroupCoordinator",
    "RankProposal",
]
