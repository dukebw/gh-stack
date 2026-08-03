package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/github/gh-stack/internal/config"
	"github.com/github/gh-stack/internal/git"
	"github.com/github/gh-stack/internal/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func managedBranch(seed string) string {
	return "user/stack/" + strings.Repeat(seed, 32)
}

func testManifest(branches ...string) reconcileManifest {
	layers := make([]reconcileManifestLayer, len(branches))
	for i, branch := range branches {
		layers[i] = reconcileManifestLayer{Branch: branch, Title: fmt.Sprintf("Layer %d", i+1), Body: fmt.Sprintf("Body %d", i+1)}
	}
	return reconcileManifest{Version: 1, Base: "master", Remote: "origin", Layers: layers}
}

func TestValidateReconcileManifest(t *testing.T) {
	valid := testManifest(managedBranch("a"))
	tests := []struct {
		name    string
		mutate  func(*reconcileManifest)
		wantErr string
	}{
		{name: "valid", mutate: func(*reconcileManifest) {}},
		{name: "version", mutate: func(m *reconcileManifest) { m.Version = 2 }, wantErr: "unsupported manifest version"},
		{name: "base", mutate: func(m *reconcileManifest) { m.Base = "" }, wantErr: "base is required"},
		{name: "remote", mutate: func(m *reconcileManifest) { m.Remote = "" }, wantErr: "remote is required"},
		{name: "managed branch", mutate: func(m *reconcileManifest) { m.Layers[0].Branch = "feature" }, wantErr: "must match"},
		{name: "title", mutate: func(m *reconcileManifest) { m.Layers[0].Title = "" }, wantErr: "title is required"},
		{name: "duplicate", mutate: func(m *reconcileManifest) { m.Layers = append(m.Layers, m.Layers[0]) }, wantErr: "duplicate layer branch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := valid
			manifest.Layers = append([]reconcileManifestLayer(nil), valid.Layers...)
			tt.mutate(&manifest)
			err := validateReconcileManifest(manifest)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}

func TestReconcileProtocolVersion(t *testing.T) {
	cfg, outR, _ := config.NewTestConfig()
	cmd := ReconcileCmd(cfg)
	cmd.SetArgs([]string{"--protocol-version"})

	require.NoError(t, cmd.Execute())
	require.NoError(t, cfg.Out.Close())
	stdout, err := io.ReadAll(outR)
	require.NoError(t, err)
	assert.Equal(t, "1\n", string(stdout))
}

type reconcileTestState struct {
	prs          map[string]*github.PullRequest
	stacks       []github.RemoteStack
	nextPR       int
	nextStack    int
	calls        []string
	createdDraft []bool
}

func newReconcileTestState() *reconcileTestState {
	return &reconcileTestState{prs: make(map[string]*github.PullRequest), nextPR: 100, nextStack: 10}
}

func (s *reconcileTestState) addPR(branch, base, state string) *github.PullRequest {
	s.nextPR++
	pr := &github.PullRequest{
		ID:          fmt.Sprintf("PR_%d", s.nextPR),
		Number:      s.nextPR,
		URL:         fmt.Sprintf("https://github.com/o/r/pull/%d", s.nextPR),
		State:       state,
		HeadRefName: branch,
		HeadRefOID:  "sha-" + branch,
		BaseRefName: base,
	}
	s.prs[branch] = pr
	return pr
}

func (s *reconcileTestState) addStack(base string, prs ...*github.PullRequest) github.RemoteStack {
	s.nextStack++
	stack := github.RemoteStack{
		ID:     s.nextStack * 10,
		Number: s.nextStack,
		URL:    fmt.Sprintf("https://api.github.com/repos/o/r/stacks/%d", s.nextStack),
		Base:   github.RemoteStackBase{Ref: base},
	}
	for _, pr := range prs {
		stack.PullRequests = append(stack.PullRequests, pr.Number)
		stack.PRDetails = append(stack.PRDetails, github.RemoteStackPR{
			Number: pr.Number,
			State:  strings.ToLower(pr.State),
			Draft:  pr.IsDraft,
			Head:   github.RemoteStackPRHead{Ref: pr.HeadRefName, Sha: pr.HeadRefOID},
		})
	}
	s.stacks = append(s.stacks, stack)
	return stack
}

func (s *reconcileTestState) client() *github.MockClient {
	return &github.MockClient{
		FindPRForBranchAllStatesFn: func(branch string) (*github.PullRequest, error) {
			return s.prs[branch], nil
		},
		FindPRByNumberFn: func(number int) (*github.PullRequest, error) {
			for _, pr := range s.prs {
				if pr.Number == number {
					return pr, nil
				}
			}
			return nil, nil
		},
		ListStacksFn: func() ([]github.RemoteStack, error) {
			return append([]github.RemoteStack(nil), s.stacks...), nil
		},
		FindStackForPRFn: func(number int) (*github.RemoteStack, error) {
			for i := range s.stacks {
				for _, candidate := range s.stacks[i].PRNumbers() {
					if candidate == number {
						return &s.stacks[i], nil
					}
				}
			}
			return nil, nil
		},
		CreatePRFn: func(base, head, title, body string, draft bool) (*github.PullRequest, error) {
			s.calls = append(s.calls, "create_pr:"+head)
			s.createdDraft = append(s.createdDraft, draft)
			pr := s.addPR(head, base, "OPEN")
			pr.Title, pr.Body, pr.IsDraft = title, body, draft
			return pr, nil
		},
		UpdatePRBaseFn: func(number int, base string) error {
			s.calls = append(s.calls, fmt.Sprintf("update_base:%d:%s", number, base))
			for _, pr := range s.prs {
				if pr.Number == number {
					pr.BaseRefName = base
				}
			}
			return nil
		},
		UpdatePRMetadataFn: func(number int, title, body string) error {
			s.calls = append(s.calls, fmt.Sprintf("update_metadata:%d:%s:%s", number, title, body))
			for _, pr := range s.prs {
				if pr.Number == number {
					pr.Title = title
					pr.Body = body
				}
			}
			return nil
		},
		ReopenPRFn: func(number int) error {
			s.calls = append(s.calls, fmt.Sprintf("reopen:%d", number))
			for _, pr := range s.prs {
				if pr.Number == number {
					pr.State = "OPEN"
				}
			}
			return nil
		},
		ClosePRFn: func(number int) error {
			s.calls = append(s.calls, fmt.Sprintf("close:%d", number))
			for _, pr := range s.prs {
				if pr.Number == number {
					pr.State = "CLOSED"
				}
			}
			return nil
		},
		MarkPRReadyForReviewFn: func(id string) error {
			s.calls = append(s.calls, "ready:"+id)
			for _, pr := range s.prs {
				if pr.ID == id {
					pr.IsDraft = false
				}
			}
			return nil
		},
		DisableAutoMergeFn: func(id string) error {
			s.calls = append(s.calls, "disable_auto_merge:"+id)
			return nil
		},
		CreateStackFn: func(numbers []int) (*github.RemoteStack, error) {
			s.calls = append(s.calls, fmt.Sprintf("create_stack:%v", numbers))
			prs := make([]*github.PullRequest, 0, len(numbers))
			for _, number := range numbers {
				for _, pr := range s.prs {
					if pr.Number == number {
						prs = append(prs, pr)
					}
				}
			}
			s.addStack(prs[0].BaseRefName, prs...)
			return &s.stacks[len(s.stacks)-1], nil
		},
		AddToStackFn: func(number int, delta []int) (*github.RemoteStack, error) {
			s.calls = append(s.calls, fmt.Sprintf("add_stack:%d:%v", number, delta))
			for i := range s.stacks {
				if s.stacks[i].Number == number {
					s.stacks[i].PullRequests = append(s.stacks[i].PullRequests, delta...)
					return &s.stacks[i], nil
				}
			}
			return nil, fmt.Errorf("stack not found")
		},
		UnstackFn: func(number int) (*github.RemoteStack, bool, error) {
			s.calls = append(s.calls, fmt.Sprintf("unstack:%d", number))
			for i := range s.stacks {
				if s.stacks[i].Number == number {
					s.stacks = append(s.stacks[:i], s.stacks[i+1:]...)
					return nil, true, nil
				}
			}
			return nil, true, nil
		},
	}
}

func mockReconcileGit(t *testing.T, shas map[string]string, calls *[]string) {
	t.Helper()
	restore := git.SetOps(&git.MockOps{
		BranchExistsFn: func(branch string) bool { _, ok := shas[branch]; return ok },
		RevParseFn:     func(branch string) (string, error) { return shas[branch], nil },
		FetchBranchesFn: func(remote string, branches []string) error {
			*calls = append(*calls, fmt.Sprintf("fetch:%s:%v", remote, branches))
			return nil
		},
		PushFn: func(remote string, branches []string, force, atomic bool) error {
			*calls = append(*calls, fmt.Sprintf("push:%s:%v:%t:%t", remote, branches, force, atomic))
			return nil
		},
		FetchBranchFn: func(remote, branch string) error {
			*calls = append(*calls, "fetch_removed:"+branch)
			return nil
		},
		DeleteRemoteBranchFn: func(remote, branch string) error {
			*calls = append(*calls, "delete_remote:"+branch)
			return nil
		},
	})
	t.Cleanup(restore)
}

func TestPlanReconcileDryRunDoesNotRequireLocalBranches(t *testing.T) {
	state := newReconcileTestState()
	manifest := testManifest(managedBranch("a"), managedBranch("b"))
	mockReconcileGit(t, nil, &state.calls)

	plan, err := planReconcile(manifest, true, state.client())
	require.NoError(t, err)
	assert.Empty(t, state.calls)
	require.Len(t, plan.result.Layers, 2)
	assert.Equal(t, "create", plan.result.Layers[0].Action)
	assert.Contains(t, actionTypes(plan.result.Actions), "create_stack")
}

func TestPlanReconcileTotalRewriteDoesNotAdoptUnidentifiedStack(t *testing.T) {
	state := newReconcileTestState()
	oldA := state.addPR(managedBranch("a"), "master", "OPEN")
	oldB := state.addPR(managedBranch("b"), oldA.HeadRefName, "OPEN")
	state.addStack("master", oldA, oldB)
	manifest := testManifest(managedBranch("c"), managedBranch("d"))
	mockReconcileGit(t, nil, &state.calls)

	plan, err := planReconcile(manifest, true, state.client())

	require.NoError(t, err)
	assert.Nil(t, plan.target)
	assert.Empty(t, plan.removed)
	assert.Contains(t, actionTypes(plan.result.Actions), "create_stack")
}

func TestApplyReconcileInitialCreateAndIdempotentRerun(t *testing.T) {
	state := newReconcileTestState()
	branches := []string{managedBranch("a"), managedBranch("b")}
	manifest := testManifest(branches...)
	manifest.Layers[1].Draft = true
	shas := map[string]string{branches[0]: "sha-" + branches[0], branches[1]: "sha-" + branches[1]}
	mockReconcileGit(t, shas, &state.calls)
	client := state.client()

	plan, err := planReconcile(manifest, false, client)
	require.NoError(t, err)
	require.NoError(t, applyReconcile(plan, client))
	assert.Equal(t, []bool{true, true}, state.createdDraft)
	assert.Contains(t, state.calls, "create_pr:"+branches[0])
	assert.Contains(t, state.calls, "create_pr:"+branches[1])
	assert.Contains(t, state.calls, "ready:PR_101")
	assert.Contains(t, state.calls, "update_metadata:101:Layer 1:Body 1")
	assert.NotContains(t, state.calls, "ready:PR_102")
	assert.NotContains(t, state.calls, "update_metadata:102:")
	assert.Contains(t, state.calls, "create_stack:[101 102]")
	require.NotNil(t, plan.result.Stack)
	assert.Equal(t, "open", plan.result.Layers[0].Status)
	assert.False(t, plan.result.Layers[0].PR.Draft)
	assert.True(t, plan.result.Layers[1].PR.Draft)

	state.calls = nil
	second, err := planReconcile(manifest, false, client)
	require.NoError(t, err)
	require.NoError(t, applyReconcile(second, client))
	assert.NotContains(t, strings.Join(state.calls, "\n"), "create_pr")
	assert.NotContains(t, strings.Join(state.calls, "\n"), "create_stack")
	assert.NotContains(t, strings.Join(state.calls, "\n"), "add_stack")
	assert.NotContains(t, strings.Join(state.calls, "\n"), "ready:")
	assert.NotContains(t, strings.Join(state.calls, "\n"), "update_metadata:")
}

func TestPlanReconcileReportsMetadataUpdate(t *testing.T) {
	state := newReconcileTestState()
	manifest := testManifest(managedBranch("a"))
	pr := state.addPR(manifest.Layers[0].Branch, "master", "OPEN")
	pr.Title = manifest.Layers[0].Title
	pr.Body = "Outdated body"
	mockReconcileGit(t, nil, &state.calls)

	plan, err := planReconcile(manifest, true, state.client())

	require.NoError(t, err)
	assert.Equal(t, "update", plan.result.Layers[0].Action)
	assert.Contains(t, actionTypes(plan.result.Actions), "update")
	assert.Empty(t, state.calls)
}

func TestApplyReconcileResumesInterruptedReadyPublication(t *testing.T) {
	state := newReconcileTestState()
	branch := managedBranch("a")
	pr := state.addPR(branch, "master", "OPEN")
	pr.IsDraft = true
	pr.Title = reconcilePendingTitlePrefix + "Layer 1"
	mockReconcileGit(t, map[string]string{branch: pr.HeadRefOID}, &state.calls)

	plan, err := planReconcile(testManifest(branch), false, state.client())
	require.NoError(t, err)
	require.NoError(t, applyReconcile(plan, state.client()))

	assert.Contains(t, state.calls, "ready:"+pr.ID)
	assert.Contains(t, state.calls, fmt.Sprintf("update_metadata:%d:Layer 1:Body 1", pr.Number))
	assert.False(t, pr.IsDraft)
	assert.Equal(t, "Layer 1", pr.Title)
	assert.Equal(t, "Body 1", pr.Body)
}

func TestApplyReconcilePreservesExistingDraftStateWhileSyncingMetadata(t *testing.T) {
	state := newReconcileTestState()
	branch := managedBranch("a")
	pr := state.addPR(branch, "master", "OPEN")
	pr.IsDraft = true
	pr.Title = "User-managed title"
	pr.Body = "User-managed body"
	mockReconcileGit(t, map[string]string{branch: pr.HeadRefOID}, &state.calls)

	plan, err := planReconcile(testManifest(branch), false, state.client())
	require.NoError(t, err)
	require.NoError(t, applyReconcile(plan, state.client()))

	assert.NotContains(t, strings.Join(state.calls, "\n"), "ready:")
	assert.Contains(t, state.calls, fmt.Sprintf("update_metadata:%d:Layer 1:Body 1", pr.Number))
	assert.True(t, pr.IsDraft)
	assert.Equal(t, "Layer 1", pr.Title)
	assert.Equal(t, "Body 1", pr.Body)
}

func TestApplyReconcileReopensClosedDesiredPR(t *testing.T) {
	state := newReconcileTestState()
	branch := managedBranch("a")
	pr := state.addPR(branch, "master", "CLOSED")
	mockReconcileGit(t, map[string]string{branch: pr.HeadRefOID}, &state.calls)
	plan, err := planReconcile(testManifest(branch), false, state.client())
	require.NoError(t, err)
	assert.Equal(t, "reopen", plan.result.Layers[0].Action)
	require.NoError(t, applyReconcile(plan, state.client()))
	assert.Contains(t, state.calls, fmt.Sprintf("reopen:%d", pr.Number))
	assert.Equal(t, "OPEN", pr.State)
	assert.Nil(t, plan.result.Stack)
}

func TestApplyReconcileOneLayerDissolvesNativeStack(t *testing.T) {
	state := newReconcileTestState()
	keep := state.addPR(managedBranch("a"), "master", "OPEN")
	remove := state.addPR(managedBranch("b"), keep.HeadRefName, "OPEN")
	stack := state.addStack("master", keep, remove)
	mockReconcileGit(t, map[string]string{keep.HeadRefName: keep.HeadRefOID}, &state.calls)

	plan, err := planReconcile(testManifest(keep.HeadRefName), false, state.client())
	require.NoError(t, err)
	require.NoError(t, applyReconcile(plan, state.client()))
	assert.Contains(t, state.calls, fmt.Sprintf("close:%d", remove.Number))
	assert.Contains(t, state.calls, "delete_remote:"+remove.HeadRefName)
	assert.Contains(t, state.calls, fmt.Sprintf("unstack:%d", stack.Number))
	assert.Nil(t, plan.result.Stack)
}

func TestPlanReconcileBlocksForeignMissingLayer(t *testing.T) {
	state := newReconcileTestState()
	desired := state.addPR(managedBranch("a"), "master", "OPEN")
	foreign := state.addPR("someone/feature", desired.HeadRefName, "OPEN")
	state.addStack("master", desired, foreign)
	mockReconcileGit(t, map[string]string{desired.HeadRefName: desired.HeadRefOID}, &state.calls)

	plan, err := planReconcile(testManifest(desired.HeadRefName), false, state.client())
	require.ErrorIs(t, err, ErrInvalidArgs)
	require.Len(t, plan.result.Blockers, 1)
	assert.Equal(t, "foreign_layer", plan.result.Blockers[0].Code)
	assert.Empty(t, state.calls)
}

func TestApplyReconcileRemovesManagedLayerBeforeUnstack(t *testing.T) {
	state := newReconcileTestState()
	a := state.addPR(managedBranch("a"), "master", "OPEN")
	b := state.addPR(managedBranch("b"), a.HeadRefName, "OPEN")
	c := state.addPR(managedBranch("c"), b.HeadRefName, "OPEN")
	stack := state.addStack("master", a, b, c)
	mockReconcileGit(t, map[string]string{a.HeadRefName: a.HeadRefOID, c.HeadRefName: c.HeadRefOID}, &state.calls)

	plan, err := planReconcile(testManifest(a.HeadRefName, c.HeadRefName), false, state.client())
	require.NoError(t, err)
	require.NoError(t, applyReconcile(plan, state.client()))
	closeIndex := indexOfCall(state.calls, fmt.Sprintf("close:%d", b.Number))
	unstackIndex := indexOfCall(state.calls, fmt.Sprintf("unstack:%d", stack.Number))
	require.NotEqual(t, -1, closeIndex)
	require.NotEqual(t, -1, unstackIndex)
	assert.Less(t, closeIndex, unstackIndex)
	assert.Contains(t, state.calls, "create_stack:[101 103]")
}

func TestPlanReconcileBlocksQueuedStructuralChange(t *testing.T) {
	state := newReconcileTestState()
	a := state.addPR(managedBranch("a"), "master", "OPEN")
	b := state.addPR(managedBranch("b"), a.HeadRefName, "OPEN")
	b.MergeQueueEntry = &github.MergeQueueEntry{ID: "queue"}
	state.addStack("master", a, b)
	mockReconcileGit(t, map[string]string{a.HeadRefName: a.HeadRefOID, b.HeadRefName: b.HeadRefOID}, &state.calls)

	plan, err := planReconcile(testManifest(b.HeadRefName, a.HeadRefName), false, state.client())
	require.ErrorIs(t, err, ErrInvalidArgs)
	assert.Contains(t, blockerCodes(plan.result.Blockers), "queued_structural_change")
	assert.Contains(t, actionTypes(plan.result.Actions), "blocked")
	assert.Equal(t, "blocked", plan.result.Layers[0].Action)
	assert.Empty(t, state.calls)
}

func TestPlanReconcileBlocksDesiredPRsAcrossStacks(t *testing.T) {
	state := newReconcileTestState()
	a := state.addPR(managedBranch("a"), "master", "OPEN")
	b := state.addPR(managedBranch("b"), "master", "OPEN")
	state.addStack("master", a)
	state.addStack("master", b)
	mockReconcileGit(t, map[string]string{a.HeadRefName: a.HeadRefOID, b.HeadRefName: b.HeadRefOID}, &state.calls)

	plan, err := planReconcile(testManifest(a.HeadRefName, b.HeadRefName), false, state.client())
	require.ErrorIs(t, err, ErrDisambiguate)
	assert.Contains(t, blockerCodes(plan.result.Blockers), "multiple_stacks")
	assert.Empty(t, state.calls)
}

func TestPlanReconcileFindsKnownStackWithoutRepositoryWideListing(t *testing.T) {
	state := newReconcileTestState()
	a := state.addPR(managedBranch("a"), "master", "OPEN")
	b := state.addPR(managedBranch("b"), a.HeadRefName, "OPEN")
	stack := state.addStack("master", a, b)
	mockReconcileGit(t, map[string]string{a.HeadRefName: a.HeadRefOID, b.HeadRefName: b.HeadRefOID}, &state.calls)
	client := state.client()
	client.ListStacksFn = func() ([]github.RemoteStack, error) {
		return nil, fmt.Errorf("repository-wide listing must not be used")
	}

	plan, err := planReconcile(testManifest(a.HeadRefName, b.HeadRefName), false, client)

	require.NoError(t, err)
	require.NotNil(t, plan.target)
	assert.Equal(t, stack.Number, plan.target.Number)
}

func TestApplyReconcileAppendsToExactStackPrefix(t *testing.T) {
	state := newReconcileTestState()
	a := state.addPR(managedBranch("a"), "master", "OPEN")
	b := state.addPR(managedBranch("b"), a.HeadRefName, "OPEN")
	stack := state.addStack("master", a, b)
	c := managedBranch("c")
	mockReconcileGit(t, map[string]string{a.HeadRefName: a.HeadRefOID, b.HeadRefName: b.HeadRefOID, c: "sha-" + c}, &state.calls)

	plan, err := planReconcile(testManifest(a.HeadRefName, b.HeadRefName, c), false, state.client())
	require.NoError(t, err)
	assert.True(t, plan.appendOnly)
	require.NoError(t, applyReconcile(plan, state.client()))
	assert.NotContains(t, strings.Join(state.calls, "\n"), "unstack:")
	assert.Contains(t, state.calls, fmt.Sprintf("add_stack:%d:[103]", stack.Number))
}

func TestPlanReconcileReportsMergedWithoutMutation(t *testing.T) {
	state := newReconcileTestState()
	branch := managedBranch("a")
	pr := state.addPR(branch, "master", "MERGED")
	pr.Merged = true
	mockReconcileGit(t, nil, &state.calls)

	plan, err := planReconcile(testManifest(branch), false, state.client())
	require.NoError(t, err)
	require.NoError(t, applyReconcile(plan, state.client()))
	assert.Equal(t, "merged", plan.result.Layers[0].Status)
	assert.Equal(t, "merged", plan.result.Layers[0].Action)
	assert.Empty(t, state.calls)
}

func TestApplyReconcileRebuildsActiveSuffixAfterPartialMerge(t *testing.T) {
	state := newReconcileTestState()
	a := state.addPR(managedBranch("a"), "master", "MERGED")
	a.Merged = true
	b := state.addPR(managedBranch("b"), "master", "OPEN")
	c := state.addPR(managedBranch("c"), b.HeadRefName, "OPEN")
	old := state.addStack("master", b, c)
	mockReconcileGit(t, map[string]string{b.HeadRefName: b.HeadRefOID, c.HeadRefName: c.HeadRefOID}, &state.calls)

	plan, err := planReconcile(testManifest(a.HeadRefName, b.HeadRefName, c.HeadRefName), false, state.client())
	require.NoError(t, err)
	assert.True(t, plan.structural)
	require.NoError(t, applyReconcile(plan, state.client()))

	assert.Contains(t, state.calls, fmt.Sprintf("unstack:%d", old.Number))
	assert.Contains(t, state.calls, "create_stack:[102 103]")
	assert.NotContains(t, strings.Join(state.calls, "\n"), a.HeadRefName)
	assert.Equal(t, "merged", plan.result.Layers[0].Status)
}

func TestApplyReconcileReordersAndReplacesStack(t *testing.T) {
	state := newReconcileTestState()
	a := state.addPR(managedBranch("a"), "master", "OPEN")
	b := state.addPR(managedBranch("b"), a.HeadRefName, "OPEN")
	old := state.addStack("master", a, b)
	mockReconcileGit(t, map[string]string{a.HeadRefName: a.HeadRefOID, b.HeadRefName: b.HeadRefOID}, &state.calls)

	plan, err := planReconcile(testManifest(b.HeadRefName, a.HeadRefName), false, state.client())
	require.NoError(t, err)
	assert.True(t, plan.structural)
	require.NoError(t, applyReconcile(plan, state.client()))
	assert.Contains(t, state.calls, fmt.Sprintf("unstack:%d", old.Number))
	assert.Contains(t, state.calls, fmt.Sprintf("update_base:%d:master", b.Number))
	assert.Contains(t, state.calls, fmt.Sprintf("update_base:%d:%s", a.Number, b.HeadRefName))
	assert.Contains(t, state.calls, "create_stack:[102 101]")
}

func TestRunReconcileJSONStdoutContainsOnlyResult(t *testing.T) {
	state := newReconcileTestState()
	manifest := testManifest(managedBranch("a"), managedBranch("b"))
	manifestJSON, err := json.Marshal(manifest)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "manifest.json")
	require.NoError(t, os.WriteFile(path, manifestJSON, 0o600))
	mockReconcileGit(t, nil, &state.calls)
	cfg, outR, errR := config.NewTestConfig()
	cfg.GitHubClientOverride = state.client()

	err = runReconcile(cfg, &reconcileOptions{input: path, json: true, dryRun: true})
	require.NoError(t, err)
	require.NoError(t, cfg.Out.Close())
	require.NoError(t, cfg.Err.Close())
	stdout, readErr := io.ReadAll(outR)
	require.NoError(t, readErr)
	stderr, readErr := io.ReadAll(errR)
	require.NoError(t, readErr)
	assert.Empty(t, stderr)
	var result reconcileResult
	require.NoError(t, json.Unmarshal(stdout, &result), "stdout must contain only JSON: %s", stdout)
	assert.Equal(t, 1, result.Version)
	assert.True(t, result.DryRun)
	require.Len(t, result.Layers, 2)
	assert.NotNil(t, result.Actions)
	assert.NotNil(t, result.Blockers)
	assert.NotNil(t, result.RemovedManaged)
}

func TestRunReconcileBlockedDryRunStillReturnsJSONPlan(t *testing.T) {
	state := newReconcileTestState()
	desired := state.addPR(managedBranch("a"), "master", "OPEN")
	foreign := state.addPR("someone/feature", desired.HeadRefName, "OPEN")
	state.addStack("master", desired, foreign)
	manifestJSON, err := json.Marshal(testManifest(desired.HeadRefName))
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "manifest.json")
	require.NoError(t, os.WriteFile(path, manifestJSON, 0o600))
	mockReconcileGit(t, nil, &state.calls)
	cfg, outR, errR := config.NewTestConfig()
	cfg.GitHubClientOverride = state.client()

	err = runReconcile(cfg, &reconcileOptions{input: path, json: true, dryRun: true})

	require.ErrorIs(t, err, ErrInvalidArgs)
	require.NoError(t, cfg.Out.Close())
	require.NoError(t, cfg.Err.Close())
	stdout, readErr := io.ReadAll(outR)
	require.NoError(t, readErr)
	stderr, readErr := io.ReadAll(errR)
	require.NoError(t, readErr)
	var result reconcileResult
	require.NoError(t, json.Unmarshal(stdout, &result), "stdout must contain JSON: %s", stdout)
	require.Len(t, result.Blockers, 1)
	assert.Equal(t, "foreign_layer", result.Blockers[0].Code)
	assert.Contains(t, string(stderr), "foreign missing branch")
}

func actionTypes(actions []reconcileAction) []string {
	result := make([]string, len(actions))
	for i, action := range actions {
		result[i] = action.Type
	}
	return result
}

func blockerCodes(blockers []reconcileBlocker) []string {
	result := make([]string, len(blockers))
	for i, blocker := range blockers {
		result[i] = blocker.Code
	}
	return result
}

func indexOfCall(calls []string, want string) int {
	for i, call := range calls {
		if call == want {
			return i
		}
	}
	return -1
}
