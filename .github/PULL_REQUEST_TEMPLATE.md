<!--  Thanks for sending a pull request!  Here are some tips for you:

1. Please read our contributor guidelines: https://github.com/gpustack/.github/blob/main/CONTRIBUTING.md 
2. Please read our code of conduct https://github.com/gpustack/.github/blob/main/CODE_OF_CONDUCT.md
3. Please label this pull request according to what type of issue you are addressing, especially if this is a release targeted pull request.
4. Ensure you have added or ran the appropriate tests for your PR.
5. If the PR is unfinished, open it as a Draft — GitHub blocks merging a draft and shows reviewers it is not ready. Convert it to a regular PR when it is ready for review. Do not prefix the title with "WIP:" and do not look for a "WIP" label: no such label exists here, and a squash merge makes the title main's commit subject, where the release-note category comes from an anchored Conventional-Commit regex — a prefixed title lands under "Other".
-->

#### What type of PR is this?

<!--
Add one of the following kinds:
/kind bug
/kind cleanup
/kind documentation
/kind enhancement

Optionally add one or more of the following kinds if applicable:
/kind api-change
/kind deprecation
/kind failing-test
/kind regression

Please also consider setting the area:
/area worker
/area workergateway
/area devicemanager
/area integrations
/area testing

And, if the change is specific to one vendor's hardware, the vendor axis:
/area vendor/<nvidia|ascend|amd|hygon|thead|cambricon|iluvatar|metax|mthreads>

TEMPORARY — delete this paragraph when Prow is enabled: none of the commands above apply a label
yet, because Prow is not installed on this repository. Write them anyway so the PR records what it
is; until Prow lands a maintainer applies the labels by hand.
-->

#### What this PR does / why we need it:

#### Which issue(s) this PR links to:
<!--
Keep the line below that matches what this PR actually does to the issue, and delete the other two.
Each verb takes an issue number or a pasted issue link, one issue per line.

`Fixes #<n>`     — the issue is fully resolved by this PR; GitHub closes it on merge.
`Addresses #<n>` — this PR advances the issue but does not finish it; no auto-close.
`Relates #<n>`   — context only, no resolution claimed; no auto-close.

The mechanism: GitHub auto-closes a linked issue only on `Fix(es|ed)`, `Close(s|d)` or
`Resolve(s|d)`. That is precisely why `Addresses` and `Relates` are safe on unfinished work — they
link the issue and leave it open.

_If PR is about `failing-tests or flakes`, please post the related issues/tests in a comment and do not use `Fixes`_
-->
Fixes #
Addresses #
Relates #

#### Special notes for your reviewer:

#### Does this PR introduce a user-facing change?
<!--
If no, just write "NONE" in the release-note block below.
If yes, a release note is required:
Enter your extended release note in the block below. If the PR requires additional action from users switching to the new release, include the string "action required".
-->
```release-note

```
