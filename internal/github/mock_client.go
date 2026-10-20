package github

// MockClient is a test double for GitHub API operations.
// Each field is an optional function that, when set, handles the corresponding
// ClientOps method call. When nil, a reasonable default is returned.
type MockClient struct {
	FindPRForBranchFn             func(string) (*PullRequest, error)
	FindPRsForBranchesAllStatesFn func([]string) ([]*PullRequest, error)
	FindPRByNumberFn              func(int) (*PullRequest, error)
	FindPRDetailsForBranchFn      func(string) (*PRDetails, error)
	CreatePRFn                    func(string, string, string, string, bool) (*PullRequest, error)
	CreatePRsFn                   func([]NewPR) ([]*PullRequest, error)
	UpdatePRBaseFn                func(int, string) error
	UpdatePRsMetadataFn           func([]PRMetadata) error
	ReopenPRFn                    func(int) error
	ClosePRFn                     func(int) error
	MarkPRReadyForReviewFn        func(string) error
	MarkPRsReadyForReviewFn       func([]*PullRequest) error
	DisableAutoMergeFn            func(string) error
	ListStacksFn                  func() ([]RemoteStack, error)
	ProbeStacksFn                 func() error
	FindStackForPRFn              func(int) (*RemoteStack, error)
	GetStackFn                    func(int) (*RemoteStack, error)
	CreateStackFn                 func([]int) (*RemoteStack, error)
	AddToStackFn                  func(int, []int) (*RemoteStack, error)
	UnstackFn                     func(int) (*RemoteStack, bool, error)
	RepoMergeConfigFn             func() (*RepoMergeConfig, error)
	MergeStackAsyncFn             func(int, string, string) (*AsyncMergeResult, error)
	GetAsyncMergeResultFn         func(int, string) (*AsyncMergeResult, error)
	PRTitlesFn                    func([]int) (map[int]string, error)
	BaseBranchUsesMergeQueueFn    func(string) (bool, error)
}

// Compile-time check that MockClient satisfies ClientOps.
var _ ClientOps = (*MockClient)(nil)

func (m *MockClient) FindPRForBranch(branch string) (*PullRequest, error) {
	if m.FindPRForBranchFn != nil {
		return m.FindPRForBranchFn(branch)
	}
	return nil, nil
}

func (m *MockClient) FindPRsForBranchesAllStates(branches []string) ([]*PullRequest, error) {
	if m.FindPRsForBranchesAllStatesFn != nil {
		return m.FindPRsForBranchesAllStatesFn(branches)
	}
	prs := make([]*PullRequest, len(branches))
	for i, branch := range branches {
		pr, err := m.FindPRForBranch(branch)
		if err != nil {
			return nil, err
		}
		prs[i] = pr
	}
	return prs, nil
}

func (m *MockClient) FindPRByNumber(number int) (*PullRequest, error) {
	if m.FindPRByNumberFn != nil {
		return m.FindPRByNumberFn(number)
	}
	return nil, nil
}

func (m *MockClient) FindPRDetailsForBranch(branch string) (*PRDetails, error) {
	if m.FindPRDetailsForBranchFn != nil {
		return m.FindPRDetailsForBranchFn(branch)
	}
	return nil, nil
}

func (m *MockClient) CreatePR(base, head, title, body string, draft bool) (*PullRequest, error) {
	if m.CreatePRFn != nil {
		return m.CreatePRFn(base, head, title, body, draft)
	}
	return nil, nil
}

func (m *MockClient) UpdatePRBase(number int, base string) error {
	if m.UpdatePRBaseFn != nil {
		return m.UpdatePRBaseFn(number, base)
	}
	return nil
}

// CreatePRs defaults to CreatePR for each spec.
func (m *MockClient) CreatePRs(specs []NewPR) ([]*PullRequest, error) {
	if m.CreatePRsFn != nil {
		return m.CreatePRsFn(specs)
	}
	prs := make([]*PullRequest, len(specs))
	for i, spec := range specs {
		pr, err := m.CreatePR(spec.Base, spec.Head, spec.Title, spec.Body, spec.Draft)
		if err != nil {
			return prs, &MutationError{Index: i, Err: err}
		}
		prs[i] = pr
	}
	return prs, nil
}

func (m *MockClient) UpdatePRsMetadata(updates []PRMetadata) error {
	if m.UpdatePRsMetadataFn != nil {
		return m.UpdatePRsMetadataFn(updates)
	}
	return nil
}

func (m *MockClient) ReopenPR(number int) error {
	if m.ReopenPRFn != nil {
		return m.ReopenPRFn(number)
	}
	return nil
}

func (m *MockClient) ClosePR(number int) error {
	if m.ClosePRFn != nil {
		return m.ClosePRFn(number)
	}
	return nil
}

func (m *MockClient) MarkPRReadyForReview(prID string) error {
	if m.MarkPRReadyForReviewFn != nil {
		return m.MarkPRReadyForReviewFn(prID)
	}
	return nil
}

// MarkPRsReadyForReview defaults to MarkPRReadyForReview for each PR.
func (m *MockClient) MarkPRsReadyForReview(prs []*PullRequest) error {
	if m.MarkPRsReadyForReviewFn != nil {
		return m.MarkPRsReadyForReviewFn(prs)
	}
	for i, pr := range prs {
		if err := m.MarkPRReadyForReview(pr.ID); err != nil {
			return &MutationError{Index: i, Err: err}
		}
	}
	return nil
}

func (m *MockClient) DisableAutoMerge(prID string) error {
	if m.DisableAutoMergeFn != nil {
		return m.DisableAutoMergeFn(prID)
	}
	return nil
}

func (m *MockClient) ListStacks() ([]RemoteStack, error) {
	if m.ListStacksFn != nil {
		return m.ListStacksFn()
	}
	return nil, nil
}

func (m *MockClient) ProbeStacks() error {
	if m.ProbeStacksFn != nil {
		return m.ProbeStacksFn()
	}
	_, err := m.ListStacks()
	return err
}

func (m *MockClient) FindStackForPR(prNumber int) (*RemoteStack, error) {
	if m.FindStackForPRFn != nil {
		return m.FindStackForPRFn(prNumber)
	}
	return nil, nil
}

func (m *MockClient) GetStack(stackNumber int) (*RemoteStack, error) {
	if m.GetStackFn != nil {
		return m.GetStackFn(stackNumber)
	}
	return &RemoteStack{}, nil
}

func (m *MockClient) CreateStack(prNumbers []int) (*RemoteStack, error) {
	if m.CreateStackFn != nil {
		return m.CreateStackFn(prNumbers)
	}
	return &RemoteStack{}, nil
}

func (m *MockClient) AddToStack(stackNumber int, prNumbers []int) (*RemoteStack, error) {
	if m.AddToStackFn != nil {
		return m.AddToStackFn(stackNumber, prNumbers)
	}
	return &RemoteStack{}, nil
}

func (m *MockClient) Unstack(stackNumber int) (*RemoteStack, bool, error) {
	if m.UnstackFn != nil {
		return m.UnstackFn(stackNumber)
	}
	return nil, false, nil
}

func (m *MockClient) RepoMergeConfig() (*RepoMergeConfig, error) {
	if m.RepoMergeConfigFn != nil {
		return m.RepoMergeConfigFn()
	}
	return &RepoMergeConfig{
		MergeAllowed:  true,
		SquashAllowed: true,
		RebaseAllowed: true,
		DefaultMethod: MergeMethodMerge,
	}, nil
}

func (m *MockClient) MergeStackAsync(prNumber int, method, mergeAction string) (*AsyncMergeResult, error) {
	if m.MergeStackAsyncFn != nil {
		return m.MergeStackAsyncFn(prNumber, method, mergeAction)
	}
	return &AsyncMergeResult{
		Status: AsyncMergeStatusPending,
		Details: AsyncMergeDetails{
			Message:     "Merge request enqueued.",
			UUID:        "mock-uuid",
			MergeMethod: method,
		},
	}, nil
}

func (m *MockClient) GetAsyncMergeResult(prNumber int, uuid string) (*AsyncMergeResult, error) {
	if m.GetAsyncMergeResultFn != nil {
		return m.GetAsyncMergeResultFn(prNumber, uuid)
	}
	return &AsyncMergeResult{
		Status: AsyncMergeStatusMerged,
		Details: AsyncMergeDetails{
			Message: "Pull request was merged.",
			SHA:     "mockmergesha",
		},
	}, nil
}

func (m *MockClient) PRTitles(numbers []int) (map[int]string, error) {
	if m.PRTitlesFn != nil {
		return m.PRTitlesFn(numbers)
	}
	return map[int]string{}, nil
}

func (m *MockClient) BaseBranchUsesMergeQueue(baseRef string) (bool, error) {
	if m.BaseBranchUsesMergeQueueFn != nil {
		return m.BaseBranchUsesMergeQueueFn(baseRef)
	}
	return false, nil
}
