package github

// ClientOps defines the interface for GitHub API operations.
// The concrete Client type satisfies this interface.
// Tests can substitute a MockClient.
type ClientOps interface {
	FindPRForBranch(branch string) (*PullRequest, error)
	FindPRsForBranchesAllStates(branches []string) ([]*PullRequest, error)
	FindPRByNumber(number int) (*PullRequest, error)
	FindPRDetailsForBranch(branch string) (*PRDetails, error)
	CreatePR(base, head, title, body string, draft bool) (*PullRequest, error)
	CreatePRs(specs []NewPR) ([]*PullRequest, error)
	UpdatePRBase(number int, base string) error
	UpdatePRsMetadata(updates []PRMetadata) error
	ReopenPR(number int) error
	ClosePR(number int) error
	MarkPRReadyForReview(prID string) error
	MarkPRsReadyForReview(prs []*PullRequest) error
	PublicationEventCounts(ids []string) (map[string]int, error)
	DisableAutoMerge(prID string) error
	ListStacks() ([]RemoteStack, error)
	ProbeStacks() error
	FindStackForPR(prNumber int) (*RemoteStack, error)
	GetStack(stackNumber int) (*RemoteStack, error)
	CreateStack(prNumbers []int) (*RemoteStack, error)
	AddToStack(stackNumber int, prNumbers []int) (*RemoteStack, error)
	Unstack(stackNumber int) (*RemoteStack, bool, error)
	RepoMergeConfig() (*RepoMergeConfig, error)
	MergeStackAsync(prNumber int, method, mergeAction string) (*AsyncMergeResult, error)
	GetAsyncMergeResult(prNumber int, uuid string) (*AsyncMergeResult, error)
	PRTitles(numbers []int) (map[int]string, error)
	BaseBranchUsesMergeQueue(baseRef string) (bool, error)
}

// Compile-time check that Client satisfies ClientOps.
var _ ClientOps = (*Client)(nil)
