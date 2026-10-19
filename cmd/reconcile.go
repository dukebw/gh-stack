package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/github/gh-stack/internal/config"
	"github.com/github/gh-stack/internal/git"
	"github.com/github/gh-stack/internal/github"
	"github.com/spf13/cobra"
)

const reconcileProtocolVersion = 1
const reconcilePendingTitlePrefix = "[stack-pr pending] "

var managedStackBranchPattern = regexp.MustCompile(`^[^/]+/stack/[0-9a-f]{32}$`)

type reconcileOptions struct {
	input           string
	json            bool
	dryRun          bool
	protocolVersion bool
}

type reconcileManifest struct {
	Version int                      `json:"version"`
	Base    string                   `json:"base"`
	Remote  string                   `json:"remote"`
	Layers  []reconcileManifestLayer `json:"layers"`
}

type reconcileManifestLayer struct {
	Branch string `json:"branch"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Draft  bool   `json:"draft"`
}

type reconcileResult struct {
	Version        int                    `json:"version"`
	DryRun         bool                   `json:"dry_run"`
	Stack          *reconcileStackResult  `json:"stack,omitempty"`
	Layers         []reconcileLayerResult `json:"layers"`
	RemovedManaged []reconcileLayerResult `json:"removed_managed_layers"`
	Actions        []reconcileAction      `json:"actions"`
	Blockers       []reconcileBlocker     `json:"blockers"`
}

type reconcileStackResult struct {
	ID     int    `json:"id"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	Base   string `json:"base"`
}

type reconcileLayerResult struct {
	Branch string             `json:"branch"`
	Status string             `json:"status"`
	Action string             `json:"action"`
	PR     *reconcilePRResult `json:"pr,omitempty"`
}

type reconcilePRResult struct {
	ID      string `json:"id"`
	Number  int    `json:"number"`
	URL     string `json:"url"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	Merged  bool   `json:"merged"`
	Queued  bool   `json:"queued"`
	HeadSHA string `json:"head_sha"`
}

type reconcileAction struct {
	Type        string `json:"type"`
	Branch      string `json:"branch,omitempty"`
	PRNumber    int    `json:"pr_number,omitempty"`
	StackNumber int    `json:"stack_number,omitempty"`
}

type reconcileBlocker struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Branch   string `json:"branch,omitempty"`
	PRNumber int    `json:"pr_number,omitempty"`
}

type reconcileLayerPlan struct {
	manifest reconcileManifestLayer
	pr       *github.PullRequest
	localSHA string
}

type reconcilePlan struct {
	manifest   reconcileManifest
	result     reconcileResult
	layers     []reconcileLayerPlan
	target     *github.RemoteStack
	removed    []*github.PullRequest
	structural bool
	appendOnly bool
	exact      bool
}

func ReconcileCmd(cfg *config.Config) *cobra.Command {
	opts := &reconcileOptions{}
	cmd := &cobra.Command{
		Use:   "reconcile --input <path|-> [--json] [--dry-run]",
		Short: "Reconcile a GitHub stack from a manifest",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if opts.protocolVersion {
				cfg.Outf("%d\n", reconcileProtocolVersion)
				return nil
			}
			return runReconcile(cfg, opts)
		},
	}
	cmd.Flags().StringVar(&opts.input, "input", "", "Manifest path, or - for stdin")
	cmd.Flags().BoolVar(&opts.json, "json", false, "Write the versioned result as JSON")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Plan without changing Git or GitHub")
	cmd.Flags().BoolVar(&opts.protocolVersion, "protocol-version", false, "Print the reconcile protocol version")
	return cmd
}

func runReconcile(cfg *config.Config, opts *reconcileOptions) error {
	result := newReconcileResult(opts.dryRun)
	if opts.input == "" {
		return finishReconcile(cfg, opts, result, ErrInvalidArgs, "--input is required")
	}

	manifest, err := loadReconcileManifest(cfg.In, opts.input)
	if err != nil {
		return finishReconcile(cfg, opts, result, ErrInvalidArgs, err.Error())
	}
	if err := validateReconcileManifest(manifest); err != nil {
		return finishReconcile(cfg, opts, result, ErrInvalidArgs, err.Error())
	}
	if len(manifest.Layers) == 0 {
		return finishReconcile(cfg, opts, result, nil, "")
	}

	client, err := cfg.GitHubClient()
	if err != nil {
		return finishReconcile(cfg, opts, result, ErrAPIFailure, fmt.Sprintf("failed to create GitHub client: %v", err))
	}
	plan, planErr := planReconcile(manifest, opts.dryRun, client)
	if plan == nil {
		return finishReconcile(cfg, opts, result, planErr, "failed to plan reconciliation")
	}
	result = plan.result
	if planErr != nil {
		return finishReconcile(cfg, opts, result, planErr, "")
	}
	if !opts.dryRun {
		if err := applyReconcile(plan, client); err != nil {
			return finishReconcile(cfg, opts, plan.result, err, "")
		}
	}
	return finishReconcile(cfg, opts, plan.result, nil, "")
}

func newReconcileResult(dryRun bool) reconcileResult {
	return reconcileResult{
		Version:        reconcileProtocolVersion,
		DryRun:         dryRun,
		Layers:         []reconcileLayerResult{},
		RemovedManaged: []reconcileLayerResult{},
		Actions:        []reconcileAction{},
		Blockers:       []reconcileBlocker{},
	}
}

func loadReconcileManifest(stdin io.Reader, path string) (reconcileManifest, error) {
	var reader io.Reader = stdin
	var file *os.File
	if path != "-" {
		var err error
		file, err = os.Open(path)
		if err != nil {
			return reconcileManifest{}, fmt.Errorf("opening manifest: %w", err)
		}
		defer func() { _ = file.Close() }()
		reader = file
	}

	var manifest reconcileManifest
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return reconcileManifest{}, fmt.Errorf("decoding manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return reconcileManifest{}, fmt.Errorf("decoding manifest: multiple JSON values")
		}
		return reconcileManifest{}, fmt.Errorf("decoding manifest: %w", err)
	}
	return manifest, nil
}

func validateReconcileManifest(manifest reconcileManifest) error {
	if manifest.Version != reconcileProtocolVersion {
		return fmt.Errorf("unsupported manifest version %d; expected %d", manifest.Version, reconcileProtocolVersion)
	}
	if strings.TrimSpace(manifest.Base) == "" {
		return fmt.Errorf("base is required")
	}
	if strings.TrimSpace(manifest.Remote) == "" {
		return fmt.Errorf("remote is required")
	}
	seen := make(map[string]bool, len(manifest.Layers))
	for i, layer := range manifest.Layers {
		if !managedStackBranchPattern.MatchString(layer.Branch) {
			return fmt.Errorf("layers[%d].branch %q must match %s", i, layer.Branch, managedStackBranchPattern)
		}
		if seen[layer.Branch] {
			return fmt.Errorf("duplicate layer branch %q", layer.Branch)
		}
		seen[layer.Branch] = true
		if strings.TrimSpace(layer.Title) == "" {
			return fmt.Errorf("layers[%d].title is required", i)
		}
	}
	return nil
}

func planReconcile(manifest reconcileManifest, dryRun bool, client github.ClientOps) (*reconcilePlan, error) {
	plan := &reconcilePlan{manifest: manifest, result: newReconcileResult(dryRun)}
	plan.layers = make([]reconcileLayerPlan, len(manifest.Layers))
	knownPRs := make(map[int]*github.PullRequest)
	branches := make([]string, len(manifest.Layers))
	for i, layer := range manifest.Layers {
		branches[i] = layer.Branch
	}
	prs, err := client.FindPRsForBranchesAllStates(branches)
	if err != nil {
		plan.addBlocker("api_error", fmt.Sprintf("failed to look up PRs for %d layer branches: %v", len(branches), err), "", 0)
		return plan, ErrAPIFailure
	}
	for i, layer := range manifest.Layers {
		pr := prs[i]
		plan.layers[i] = reconcileLayerPlan{manifest: layer, pr: pr}
		if pr != nil {
			knownPRs[pr.Number] = pr
		}
	}

	target, err := findReconcileStack(client, plan.layers)
	plan.target = target
	if err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == 404 {
			plan.addBlocker("stacks_unavailable", "stacked PRs are not enabled for this repository", "", 0)
			return plan, ErrStacksUnavailable
		}
		if errors.Is(err, errReconcileMultipleStacks) {
			plan.addBlocker("multiple_stacks", "desired pull requests belong to multiple stacks", "", 0)
			return plan, ErrDisambiguate
		}
		plan.addBlocker("api_error", fmt.Sprintf("failed to discover native stack: %v", err), "", 0)
		return plan, ErrAPIFailure
	}

	targetPRs := make([]*github.PullRequest, 0)
	if plan.target != nil {
		for i, number := range plan.target.PRNumbers() {
			pr := knownPRs[number]
			if pr == nil {
				pr, err = client.FindPRByNumber(number)
				if err != nil || pr == nil {
					plan.addBlocker("api_error", fmt.Sprintf("failed to resolve PR #%d in stack #%d", number, plan.target.Number), "", number)
					return plan, ErrAPIFailure
				}
			}
			if pr.HeadRefName == "" && i < len(plan.target.PRDetails) {
				pr.HeadRefName = plan.target.PRDetails[i].Head.Ref
			}
			if pr.HeadRefOID == "" && i < len(plan.target.PRDetails) {
				pr.HeadRefOID = plan.target.PRDetails[i].Head.Sha
			}
			targetPRs = append(targetPRs, pr)
		}
	}

	desiredByNumber := make(map[int]bool, len(knownPRs))
	for number := range knownPRs {
		desiredByNumber[number] = true
	}
	for _, pr := range targetPRs {
		if desiredByNumber[pr.Number] {
			continue
		}
		if !managedStackBranchPattern.MatchString(pr.HeadRefName) {
			plan.addBlocker("foreign_layer", fmt.Sprintf("stack #%d contains foreign missing branch %s", plan.target.Number, pr.HeadRefName), pr.HeadRefName, pr.Number)
			continue
		}
		plan.removed = append(plan.removed, pr)
	}

	currentBranches := make([]string, len(targetPRs))
	for i, pr := range targetPRs {
		currentBranches[i] = pr.HeadRefName
	}
	desiredBranches := make([]string, len(plan.layers))
	allDesiredExist := true
	for i := range plan.layers {
		desiredBranches[i] = plan.layers[i].manifest.Branch
		if plan.layers[i].pr == nil {
			allDesiredExist = false
		}
	}
	baseMatches := plan.target != nil && plan.target.Base.Ref == manifest.Base
	plan.exact = plan.target != nil && baseMatches && allDesiredExist && stringSlicesEqual(currentBranches, desiredBranches)
	plan.appendOnly = plan.target != nil && baseMatches && len(plan.removed) == 0 &&
		len(currentBranches) < len(desiredBranches) && stringSlicePrefix(currentBranches, desiredBranches) && !containsMergedPR(targetPRs)
	plan.structural = plan.target != nil && !plan.exact && !plan.appendOnly

	activeCount := 0
	for i := range plan.layers {
		layer := &plan.layers[i]
		if isMergedPR(layer.pr) {
			continue
		}
		activeCount++
		if git.BranchExists(layer.manifest.Branch) {
			sha, err := git.RevParse(layer.manifest.Branch)
			if err != nil {
				plan.addBlocker("local_ref", fmt.Sprintf("failed to resolve local branch %s: %v", layer.manifest.Branch, err), layer.manifest.Branch, prNumber(layer.pr))
				continue
			}
			layer.localSHA = sha
		} else if !dryRun {
			plan.addBlocker("missing_local_branch", fmt.Sprintf("local branch %s does not exist", layer.manifest.Branch), layer.manifest.Branch, prNumber(layer.pr))
		}
	}
	if len(manifest.Layers) == 1 && activeCount == 1 && plan.target != nil {
		plan.structural = true
		plan.exact = false
		plan.appendOnly = false
	}

	if plan.structural {
		for _, pr := range targetPRs {
			if pr.IsQueued() {
				plan.addBlocker("queued_structural_change", fmt.Sprintf("queued PR #%d blocks a structural stack change", pr.Number), pr.HeadRefName, pr.Number)
			}
		}
	}
	for i := range plan.layers {
		layer := &plan.layers[i]
		if layer.pr == nil || !layer.pr.IsQueued() {
			continue
		}
		if layer.localSHA != "" && layer.pr.HeadRefOID != "" && layer.localSHA != layer.pr.HeadRefOID {
			plan.addBlocker("queued_branch_change", fmt.Sprintf("queued PR #%d does not match local branch %s", layer.pr.Number, layer.manifest.Branch), layer.manifest.Branch, layer.pr.Number)
		}
	}

	plan.buildResult(targetPRs)
	if len(plan.result.Blockers) > 0 {
		return plan, ErrInvalidArgs
	}
	return plan, nil
}

var errReconcileMultipleStacks = errors.New("desired pull requests belong to multiple stacks")

// findReconcileStack uses the stack membership returned with each PR, so it
// needs one REST call for the matched stack instead of one per PR. That call
// is made even when no PR is stacked: its 404 is how a repository without
// stacked PRs is detected.
func findReconcileStack(client github.ClientOps, layers []reconcileLayerPlan) (*github.RemoteStack, error) {
	probePR, stackNumber := 0, 0
	for i := range layers {
		pr := layers[i].pr
		if pr == nil {
			continue
		}
		if probePR == 0 {
			probePR = pr.Number
		}
		if pr.Stack == nil {
			continue
		}
		if stackNumber == 0 {
			probePR, stackNumber = pr.Number, pr.Stack.Number
		} else if pr.Stack.Number != stackNumber {
			return nil, errReconcileMultipleStacks
		}
	}
	if probePR == 0 {
		_, err := client.ListStacks()
		return nil, err
	}
	matched, err := client.FindStackForPR(probePR)
	if err != nil {
		return nil, err
	}
	if (matched == nil) != (stackNumber == 0) || (matched != nil && matched.Number != stackNumber) {
		return nil, fmt.Errorf("stack lookup for PR #%d disagrees with its GraphQL stack #%d", probePR, stackNumber)
	}
	return matched, nil
}

func (p *reconcilePlan) buildResult(targetPRs []*github.PullRequest) {
	if p.target != nil && !p.structural {
		p.result.Stack = stackResult(p.target)
	}
	targetNumbers := make(map[int]bool, len(targetPRs))
	for _, pr := range targetPRs {
		targetNumbers[pr.Number] = true
	}

	for i := range p.layers {
		layer := &p.layers[i]
		status, action := "new", "create"
		switch {
		case isMergedPR(layer.pr):
			status, action = "merged", "merged"
		case layer.pr != nil && layer.pr.IsQueued():
			status, action = "queued", "none"
		case layer.pr != nil && strings.EqualFold(layer.pr.State, "CLOSED"):
			status, action = "closed", "reopen"
		case layer.pr != nil:
			status, action = "open", "none"
		}
		if action == "none" && layer.pr != nil && !isMergedPR(layer.pr) && !layer.pr.IsQueued() {
			if p.structural {
				action = "reorder"
			} else if (p.target != nil && !targetNumbers[layer.pr.Number]) || layer.pr.BaseRefName != expectedActiveBase(p.layers, i, p.manifest.Base) ||
				(layer.localSHA != "" && layer.pr.HeadRefOID != "" && layer.localSHA != layer.pr.HeadRefOID) ||
				layer.pr.Title != layer.manifest.Title || layer.pr.Body != layer.manifest.Body {
				action = "update"
			}
		}
		p.result.Layers = append(p.result.Layers, reconcileLayerResult{
			Branch: layer.manifest.Branch,
			Status: status,
			Action: action,
			PR:     prResult(layer.pr, layer.localSHA),
		})
		if action != "none" {
			p.result.Actions = append(p.result.Actions, reconcileAction{Type: action, Branch: layer.manifest.Branch, PRNumber: prNumber(layer.pr)})
		}
	}

	for _, pr := range p.removed {
		action := "remove"
		status := strings.ToLower(pr.State)
		if isMergedPR(pr) {
			action = "merged"
			status = "merged"
		}
		p.result.RemovedManaged = append(p.result.RemovedManaged, reconcileLayerResult{
			Branch: pr.HeadRefName,
			Status: status,
			Action: action,
			PR:     prResult(pr, ""),
		})
		p.result.Actions = append(p.result.Actions, reconcileAction{Type: action, Branch: pr.HeadRefName, PRNumber: pr.Number})
	}

	active := p.activeLayers()
	switch {
	case p.structural && p.target != nil:
		p.result.Actions = append(p.result.Actions, reconcileAction{Type: "unstack", StackNumber: p.target.Number})
		if len(active) >= 2 {
			p.result.Actions = append(p.result.Actions, reconcileAction{Type: "reorder"})
		}
	case p.appendOnly:
		p.result.Actions = append(p.result.Actions, reconcileAction{Type: "update_stack", StackNumber: p.target.Number})
	case p.target == nil && len(active) >= 2:
		p.result.Actions = append(p.result.Actions, reconcileAction{Type: "create_stack"})
	}
	for _, blocker := range p.result.Blockers {
		p.result.Actions = append(p.result.Actions, reconcileAction{Type: "blocked", Branch: blocker.Branch, PRNumber: blocker.PRNumber})
		for i := range p.result.Layers {
			if blocker.Branch != "" && p.result.Layers[i].Branch == blocker.Branch {
				p.result.Layers[i].Action = "blocked"
			}
		}
	}
}

func (p *reconcilePlan) activeLayers() []*reconcileLayerPlan {
	active := make([]*reconcileLayerPlan, 0, len(p.layers))
	for i := range p.layers {
		if !isMergedPR(p.layers[i].pr) {
			active = append(active, &p.layers[i])
		}
	}
	return active
}

func (p *reconcilePlan) addBlocker(code, message, branch string, number int) {
	p.result.Blockers = append(p.result.Blockers, reconcileBlocker{Code: code, Message: message, Branch: branch, PRNumber: number})
}

func applyReconcile(plan *reconcilePlan, client github.ClientOps) error {
	var reconciledStack *github.RemoteStack
	if !plan.structural {
		reconciledStack = plan.target
	}
	defer func() { refreshReconcileResult(plan, reconciledStack) }()

	for _, pr := range mutablePRs(plan) {
		if pr.IsAutoMergeEnabled() {
			if err := client.DisableAutoMerge(pr.ID); err != nil {
				return failReconcile(plan, ErrAPIFailure, "api_error", fmt.Sprintf("failed to disable auto-merge for PR #%d: %v", pr.Number, err), pr.HeadRefName, pr.Number)
			}
		}
	}

	pushBranches := make([]string, 0, len(plan.layers))
	for i := range plan.layers {
		if isMergedPR(plan.layers[i].pr) || (plan.layers[i].pr != nil && plan.layers[i].pr.IsQueued()) {
			continue
		}
		pushBranches = append(pushBranches, plan.layers[i].manifest.Branch)
	}
	if len(pushBranches) > 0 {
		if err := git.FetchBranches(plan.manifest.Remote, pushBranches); err != nil {
			return failReconcile(plan, ErrSilent, "git_error", fmt.Sprintf("failed to fetch branches from %s: %v", plan.manifest.Remote, err), "", 0)
		}
		if err := git.Push(plan.manifest.Remote, pushBranches, true, true); err != nil {
			return failReconcile(plan, ErrSilent, "git_error", fmt.Sprintf("failed to push branches to %s: %v", plan.manifest.Remote, err), "", 0)
		}
	}

	for i := range plan.layers {
		pr := plan.layers[i].pr
		if pr != nil && !isMergedPR(pr) && strings.EqualFold(pr.State, "CLOSED") {
			if err := client.ReopenPR(pr.Number); err != nil {
				return failReconcile(plan, ErrAPIFailure, "api_error", fmt.Sprintf("failed to reopen PR #%d: %v", pr.Number, err), pr.HeadRefName, pr.Number)
			}
			pr.State = "OPEN"
		}
	}

	for _, pr := range plan.removed {
		if isMergedPR(pr) {
			continue
		}
		if !strings.EqualFold(pr.State, "CLOSED") {
			if err := client.ClosePR(pr.Number); err != nil {
				return failReconcile(plan, ErrAPIFailure, "api_error", fmt.Sprintf("failed to close removed PR #%d: %v", pr.Number, err), pr.HeadRefName, pr.Number)
			}
			pr.State = "CLOSED"
		}
		if err := deleteManagedRemoteBranch(plan.manifest.Remote, pr.HeadRefName); err != nil {
			return failReconcile(plan, ErrSilent, "git_error", fmt.Sprintf("failed to delete managed branch %s from %s: %v", pr.HeadRefName, plan.manifest.Remote, err), pr.HeadRefName, pr.Number)
		}
	}

	if plan.structural && plan.target != nil {
		remaining, dissolved, err := client.Unstack(plan.target.Number)
		if err != nil {
			var httpErr *api.HTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != 404 {
				return failReconcile(plan, ErrAPIFailure, "api_error", fmt.Sprintf("failed to unstack stack #%d: %v", plan.target.Number, err), "", 0)
			}
		} else if !dissolved && remaining != nil {
			activeNumbers := make(map[int]bool)
			for _, layer := range plan.activeLayers() {
				if layer.pr != nil {
					activeNumbers[layer.pr.Number] = true
				}
			}
			for _, number := range remaining.PRNumbers() {
				if activeNumbers[number] {
					return failReconcile(plan, ErrAPIFailure, "stack_locked", fmt.Sprintf("stack #%d still contains mutable PR #%d after unstacking", plan.target.Number, number), "", number)
				}
			}
		}
	}

	active := plan.activeLayers()
	for i, layer := range active {
		base := plan.manifest.Base
		if i > 0 {
			base = active[i-1].manifest.Branch
		}
		if layer.pr == nil {
			title := layer.manifest.Title
			if !layer.manifest.Draft {
				title = reconcilePendingTitlePrefix + title
			}
			pr, err := client.CreatePR(base, layer.manifest.Branch, title, layer.manifest.Body, true)
			if err != nil {
				return failReconcile(plan, ErrAPIFailure, "api_error", fmt.Sprintf("failed to create PR for %s: %v", layer.manifest.Branch, err), layer.manifest.Branch, 0)
			}
			pr.State = "OPEN"
			pr.Title = title
			pr.Body = layer.manifest.Body
			pr.HeadRefName = layer.manifest.Branch
			pr.HeadRefOID = layer.localSHA
			pr.BaseRefName = base
			pr.IsDraft = true
			layer.pr = pr
			continue
		}
		if shouldRepairBase(plan, layer.pr) && layer.pr.BaseRefName != base {
			if err := client.UpdatePRBase(layer.pr.Number, base); err != nil {
				return failReconcile(plan, ErrAPIFailure, "api_error", fmt.Sprintf("failed to update base of PR #%d to %s: %v", layer.pr.Number, base, err), layer.manifest.Branch, layer.pr.Number)
			}
			layer.pr.BaseRefName = base
		}
	}

	prNumbers := make([]int, len(active))
	for i, layer := range active {
		prNumbers[i] = layer.pr.Number
	}
	switch {
	case len(prNumbers) < 2:
	case plan.exact:
		reconciledStack = plan.target
	case plan.appendOnly:
		current := plan.target.PRNumbers()
		delta := prNumbers[len(current):]
		if len(delta) > 0 {
			updated, err := client.AddToStack(plan.target.Number, delta)
			if err != nil {
				return failReconcile(plan, stackMutationError(err), "api_error", fmt.Sprintf("failed to append to stack #%d: %v", plan.target.Number, err), "", 0)
			}
			reconciledStack = updated
		} else {
			reconciledStack = plan.target
		}
	default:
		var err error
		reconciledStack, err = client.CreateStack(prNumbers)
		if err != nil {
			return failReconcile(plan, stackMutationError(err), "api_error", fmt.Sprintf("failed to create stack: %v", err), "", 0)
		}
	}

	for _, layer := range active {
		pendingTitle := reconcilePendingTitlePrefix + layer.manifest.Title
		if layer.pr.Title == pendingTitle && !layer.manifest.Draft && layer.pr.IsDraft {
			if err := client.MarkPRReadyForReview(layer.pr.ID); err != nil {
				return failReconcile(plan, ErrAPIFailure, "api_error", fmt.Sprintf("failed to mark PR #%d ready for review: %v", layer.pr.Number, err), layer.manifest.Branch, layer.pr.Number)
			}
			layer.pr.IsDraft = false
		}
		if layer.pr.Title == layer.manifest.Title && layer.pr.Body == layer.manifest.Body {
			continue
		}
		if err := client.UpdatePRMetadata(layer.pr.Number, layer.manifest.Title, layer.manifest.Body); err != nil {
			return failReconcile(plan, ErrAPIFailure, "api_error", fmt.Sprintf("failed to synchronize metadata of PR #%d: %v", layer.pr.Number, err), layer.manifest.Branch, layer.pr.Number)
		}
		layer.pr.Title = layer.manifest.Title
		layer.pr.Body = layer.manifest.Body
	}
	return nil
}

func failReconcile(plan *reconcilePlan, exitErr error, code, message, branch string, number int) error {
	plan.addBlocker(code, message, branch, number)
	plan.result.Actions = append(plan.result.Actions, reconcileAction{Type: "blocked", Branch: branch, PRNumber: number})
	return exitErr
}

func mutablePRs(plan *reconcilePlan) []*github.PullRequest {
	seen := make(map[int]bool)
	var prs []*github.PullRequest
	for i := range plan.layers {
		pr := plan.layers[i].pr
		if pr != nil && !isMergedPR(pr) && !seen[pr.Number] {
			seen[pr.Number] = true
			prs = append(prs, pr)
		}
	}
	for _, pr := range plan.removed {
		if !isMergedPR(pr) && !seen[pr.Number] {
			seen[pr.Number] = true
			prs = append(prs, pr)
		}
	}
	return prs
}

func shouldRepairBase(plan *reconcilePlan, pr *github.PullRequest) bool {
	if plan.target == nil || plan.structural || len(plan.manifest.Layers) == 1 {
		return true
	}
	if plan.appendOnly {
		for _, number := range plan.target.PRNumbers() {
			if number == pr.Number {
				return false
			}
		}
		return true
	}
	return false
}

func deleteManagedRemoteBranch(remote, branch string) error {
	err := git.FetchBranch(remote, branch)
	if errors.Is(err, git.ErrRemoteBranchNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return git.DeleteRemoteBranch(remote, branch)
}

func stackMutationError(err error) error {
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == 404 {
		return ErrStacksUnavailable
	}
	return ErrAPIFailure
}

func refreshReconcileResult(plan *reconcilePlan, stack *github.RemoteStack) {
	if stack != nil {
		plan.result.Stack = stackResult(stack)
	} else {
		plan.result.Stack = nil
	}
	for i := range plan.layers {
		layer := &plan.layers[i]
		result := &plan.result.Layers[i]
		result.PR = prResult(layer.pr, layer.localSHA)
		switch {
		case isMergedPR(layer.pr):
			result.Status = "merged"
		case layer.pr != nil && layer.pr.IsQueued():
			result.Status = "queued"
		case layer.pr != nil:
			result.Status = "open"
		}
	}
	for i, pr := range plan.removed {
		plan.result.RemovedManaged[i].PR = prResult(pr, "")
		if !isMergedPR(pr) {
			plan.result.RemovedManaged[i].Status = "closed"
		}
	}
}

func finishReconcile(cfg *config.Config, opts *reconcileOptions, result reconcileResult, commandErr error, message string) error {
	if message != "" {
		result.Blockers = append(result.Blockers, reconcileBlocker{Code: blockerCode(commandErr), Message: message})
		cfg.Errorf("%s", message)
	} else if commandErr != nil {
		for _, blocker := range result.Blockers {
			cfg.Errorf("%s", blocker.Message)
		}
	}
	if opts.json {
		encoder := json.NewEncoder(cfg.Out)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(result); err != nil {
			cfg.Errorf("failed to encode reconciliation result: %v", err)
			return ErrSilent
		}
	} else if commandErr == nil {
		cfg.Outf("Reconciliation %s: %d layer(s), %d action(s)\n", map[bool]string{true: "plan", false: "complete"}[opts.dryRun], len(result.Layers), len(result.Actions))
	}
	return commandErr
}

func blockerCode(err error) string {
	switch {
	case errors.Is(err, ErrInvalidArgs):
		return "invalid_manifest"
	case errors.Is(err, ErrStacksUnavailable):
		return "stacks_unavailable"
	case errors.Is(err, ErrDisambiguate):
		return "multiple_stacks"
	case errors.Is(err, ErrAPIFailure):
		return "api_error"
	default:
		return "operation_failed"
	}
}

func stackResult(stack *github.RemoteStack) *reconcileStackResult {
	if stack == nil {
		return nil
	}
	return &reconcileStackResult{ID: stack.ID, Number: stack.Number, URL: stack.URL, Base: stack.Base.Ref}
}

func prResult(pr *github.PullRequest, localSHA string) *reconcilePRResult {
	if pr == nil {
		return nil
	}
	headSHA := pr.HeadRefOID
	if localSHA != "" && !pr.IsQueued() && !isMergedPR(pr) {
		headSHA = localSHA
	}
	state := strings.ToLower(pr.State)
	if state == "" {
		state = "open"
	}
	return &reconcilePRResult{
		ID:      pr.ID,
		Number:  pr.Number,
		URL:     pr.URL,
		State:   state,
		Draft:   pr.IsDraft,
		Merged:  isMergedPR(pr),
		Queued:  pr.IsQueued(),
		HeadSHA: headSHA,
	}
}

func expectedActiveBase(layers []reconcileLayerPlan, index int, base string) string {
	for i := index - 1; i >= 0; i-- {
		if !isMergedPR(layers[i].pr) {
			return layers[i].manifest.Branch
		}
	}
	return base
}

func isMergedPR(pr *github.PullRequest) bool {
	return pr != nil && (pr.Merged || strings.EqualFold(pr.State, "MERGED"))
}

func containsMergedPR(prs []*github.PullRequest) bool {
	for _, pr := range prs {
		if isMergedPR(pr) {
			return true
		}
	}
	return false
}

func prNumber(pr *github.PullRequest) int {
	if pr == nil {
		return 0
	}
	return pr.Number
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSlicePrefix(prefix, values []string) bool {
	if len(prefix) > len(values) {
		return false
	}
	for i := range prefix {
		if prefix[i] != values[i] {
			return false
		}
	}
	return true
}
