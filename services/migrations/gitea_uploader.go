// Copyright 2019 The Gitea Authors. All rights reserved.
// Copyright 2018 Jonas Franz. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package migrations

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"code.gitea.io/gitea/models"
	"code.gitea.io/gitea/models/db"
	repo_model "code.gitea.io/gitea/models/repo"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/log"
	base "code.gitea.io/gitea/modules/migration"
	repo_module "code.gitea.io/gitea/modules/repository"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/storage"
	"code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/uri"
	"code.gitea.io/gitea/services/pull"

	gouuid "github.com/google/uuid"
)

var (
	_ base.Uploader = &GiteaLocalUploader{}
)

// GiteaLocalUploader implements an Uploader to gitea sites
type GiteaLocalUploader struct {
	ctx            context.Context
	doer           *user_model.User
	repoOwner      string
	repoName       string
	repo           *repo_model.Repository
	labels         sync.Map
	milestones     sync.Map
	issues         sync.Map
	gitRepo        *git.Repository
	prHeadCache    map[string]struct{}
	userMap        map[int64]int64 // external user id mapping to user id
	prCache        map[int64]*models.PullRequest
	gitServiceType structs.GitServiceType
}

// NewGiteaLocalUploader creates an gitea Uploader via gitea API v1
func NewGiteaLocalUploader(ctx context.Context, doer *user_model.User, repoOwner, repoName string) *GiteaLocalUploader {
	return &GiteaLocalUploader{
		ctx:         ctx,
		doer:        doer,
		repoOwner:   repoOwner,
		repoName:    repoName,
		prHeadCache: make(map[string]struct{}),
		userMap:     make(map[int64]int64),
		prCache:     make(map[int64]*models.PullRequest),
	}
}

// MaxBatchInsertSize returns the table's max batch insert size
func (g *GiteaLocalUploader) MaxBatchInsertSize(tp string) int {
	switch tp {
	case "issue":
		return db.MaxBatchInsertSize(new(models.Issue))
	case "comment":
		return db.MaxBatchInsertSize(new(models.Comment))
	case "milestone":
		return db.MaxBatchInsertSize(new(models.Milestone))
	case "label":
		return db.MaxBatchInsertSize(new(models.Label))
	case "release":
		return db.MaxBatchInsertSize(new(models.Release))
	case "pullrequest":
		return db.MaxBatchInsertSize(new(models.PullRequest))
	}
	return 10
}

// CreateRepo creates a repository
func (g *GiteaLocalUploader) CreateRepo(repo *base.Repository, opts base.MigrateOptions) error {
	owner, err := user_model.GetUserByName(g.repoOwner)
	if err != nil {
		return err
	}

	var r *repo_model.Repository
	if opts.MigrateToRepoID <= 0 {
		r, err = repo_module.CreateRepository(g.doer, owner, models.CreateRepoOptions{
			Name:           g.repoName,
			Description:    repo.Description,
			OriginalURL:    repo.OriginalURL,
			GitServiceType: opts.GitServiceType,
			IsPrivate:      opts.Private,
			IsMirror:       opts.Mirror,
			Status:         repo_model.RepositoryBeingMigrated,
		})
	} else {
		r, err = repo_model.GetRepositoryByID(opts.MigrateToRepoID)
	}
	if err != nil {
		return err
	}
	r.DefaultBranch = repo.DefaultBranch
	r.Description = repo.Description

	r, err = repo_module.MigrateRepositoryGitData(g.ctx, owner, r, base.MigrateOptions{
		RepoName:       g.repoName,
		Description:    repo.Description,
		OriginalURL:    repo.OriginalURL,
		GitServiceType: opts.GitServiceType,
		Mirror:         repo.IsMirror,
		LFS:            opts.LFS,
		LFSEndpoint:    opts.LFSEndpoint,
		CloneAddr:      repo.CloneURL,
		Private:        repo.IsPrivate,
		Wiki:           opts.Wiki,
		Releases:       opts.Releases, // if didn't get releases, then sync them from tags
		MirrorInterval: opts.MirrorInterval,
		AuthUsername:   opts.AuthUsername,
		AuthPassword:   opts.AuthPassword,
	}, NewMigrationHTTPTransport())

	g.repo = r
	if err != nil {
		return err
	}
	g.gitRepo, err = git.OpenRepository(r.RepoPath())
	return err
}

// Close closes this uploader
func (g *GiteaLocalUploader) Close() {
	if g.gitRepo != nil {
		g.gitRepo.Close()
	}
}

// CreateTopics creates topics
func (g *GiteaLocalUploader) CreateTopics(topics ...string) error {
	// ignore topics to long for the db
	c := 0
	for i := range topics {
		if len(topics[i]) <= 50 {
			topics[c] = topics[i]
			c++
		}
	}
	topics = topics[:c]
	return repo_model.SaveTopics(g.repo.ID, topics...)
}

// CreateMilestones creates milestones
func (g *GiteaLocalUploader) CreateMilestones(milestones ...*base.Milestone) error {
	var mss = make([]*models.Milestone, 0, len(milestones))
	for _, milestone := range milestones {
		var deadline timeutil.TimeStamp
		if milestone.Deadline != nil {
			deadline = timeutil.TimeStamp(milestone.Deadline.Unix())
		}
		if deadline == 0 {
			deadline = timeutil.TimeStamp(time.Date(9999, 1, 1, 0, 0, 0, 0, setting.DefaultUILocation).Unix())
		}

		if milestone.Created.IsZero() {
			if milestone.Updated != nil {
				milestone.Created = *milestone.Updated
			} else if milestone.Deadline != nil {
				milestone.Created = *milestone.Deadline
			} else {
				milestone.Created = time.Now()
			}
		}
		if milestone.Updated == nil || milestone.Updated.IsZero() {
			milestone.Updated = &milestone.Created
		}

		var ms = models.Milestone{
			RepoID:       g.repo.ID,
			Name:         milestone.Title,
			Content:      milestone.Description,
			IsClosed:     milestone.State == "closed",
			CreatedUnix:  timeutil.TimeStamp(milestone.Created.Unix()),
			UpdatedUnix:  timeutil.TimeStamp(milestone.Updated.Unix()),
			DeadlineUnix: deadline,
		}
		if ms.IsClosed && milestone.Closed != nil {
			ms.ClosedDateUnix = timeutil.TimeStamp(milestone.Closed.Unix())
		}
		mss = append(mss, &ms)
	}

	err := models.InsertMilestones(mss...)
	if err != nil {
		return err
	}

	for _, ms := range mss {
		g.milestones.Store(ms.Name, ms.ID)
	}
	return nil
}

// CreateLabels creates labels
func (g *GiteaLocalUploader) CreateLabels(labels ...*base.Label) error {
	var lbs = make([]*models.Label, 0, len(labels))
	for _, label := range labels {
		lbs = append(lbs, &models.Label{
			RepoID:      g.repo.ID,
			Name:        label.Name,
			Description: label.Description,
			Color:       fmt.Sprintf("#%s", label.Color),
		})
	}

	err := models.NewLabels(lbs...)
	if err != nil {
		return err
	}
	for _, lb := range lbs {
		g.labels.Store(lb.Name, lb)
	}
	return nil
}

// CreateReleases creates releases
func (g *GiteaLocalUploader) CreateReleases(releases ...*base.Release) error {
	var rels = make([]*models.Release, 0, len(releases))
	for _, release := range releases {
		if release.Created.IsZero() {
			if !release.Published.IsZero() {
				release.Created = release.Published
			} else {
				release.Created = time.Now()
			}
		}

		var rel = models.Release{
			RepoID:       g.repo.ID,
			TagName:      release.TagName,
			LowerTagName: strings.ToLower(release.TagName),
			Target:       release.TargetCommitish,
			Title:        release.Name,
			Sha1:         release.TargetCommitish,
			Note:         release.Body,
			IsDraft:      release.Draft,
			IsPrerelease: release.Prerelease,
			IsTag:        false,
			CreatedUnix:  timeutil.TimeStamp(release.Created.Unix()),
		}

		userid, ok := g.userMap[release.PublisherID]
		tp := g.gitServiceType.Name()
		if !ok && tp != "" {
			var err error
			userid, err = user_model.GetUserIDByExternalUserID(tp, fmt.Sprintf("%v", release.PublisherID))
			if err != nil {
				log.Error("GetUserIDByExternalUserID: %v", err)
			}
			if userid > 0 {
				g.userMap[release.PublisherID] = userid
			}
		}

		if userid > 0 {
			rel.PublisherID = userid
		} else {
			rel.PublisherID = g.doer.ID
			rel.OriginalAuthor = release.PublisherName
			rel.OriginalAuthorID = release.PublisherID
		}

		// calc NumCommits if no draft
		if !release.Draft {
			commit, err := g.gitRepo.GetTagCommit(rel.TagName)
			if err != nil {
				return fmt.Errorf("GetTagCommit[%v]: %v", rel.TagName, err)
			}
			rel.NumCommits, err = commit.CommitsCount()
			if err != nil {
				return fmt.Errorf("CommitsCount: %v", err)
			}
		}

		for _, asset := range release.Assets {
			if asset.Created.IsZero() {
				if !asset.Updated.IsZero() {
					asset.Created = asset.Updated
				} else {
					asset.Created = release.Created
				}
			}
			var attach = repo_model.Attachment{
				UUID:          gouuid.New().String(),
				Name:          asset.Name,
				DownloadCount: int64(*asset.DownloadCount),
				Size:          int64(*asset.Size),
				CreatedUnix:   timeutil.TimeStamp(asset.Created.Unix()),
			}

			// download attachment
			err := func() error {
				// asset.DownloadURL maybe a local file
				var rc io.ReadCloser
				var err error
				if asset.DownloadFunc != nil {
					rc, err = asset.DownloadFunc()
					if err != nil {
						return err
					}
				} else if asset.DownloadURL != nil {
					rc, err = uri.Open(*asset.DownloadURL)
					if err != nil {
						return err
					}
				}
				if rc == nil {
					return nil
				}
				_, err = storage.Attachments.Save(attach.RelativePath(), rc, int64(*asset.Size))
				rc.Close()
				return err
			}()
			if err != nil {
				return err
			}

			rel.Attachments = append(rel.Attachments, &attach)
		}

		rels = append(rels, &rel)
	}

	return models.InsertReleases(rels...)
}

// SyncTags syncs releases with tags in the database
func (g *GiteaLocalUploader) SyncTags() error {
	return repo_module.SyncReleasesWithTags(g.repo, g.gitRepo)
}

// CreateIssues creates issues
func (g *GiteaLocalUploader) CreateIssues(issues ...*base.Issue) error {
	var iss = make([]*models.Issue, 0, len(issues))
	for _, issue := range issues {
		var labels []*models.Label
		for _, label := range issue.Labels {
			lb, ok := g.labels.Load(label.Name)
			if ok {
				labels = append(labels, lb.(*models.Label))
			}
		}

		var milestoneID int64
		if issue.Milestone != "" {
			milestone, ok := g.milestones.Load(issue.Milestone)
			if ok {
				milestoneID = milestone.(int64)
			}
		}

		if issue.Created.IsZero() {
			if issue.Closed != nil {
				issue.Created = *issue.Closed
			} else {
				issue.Created = time.Now()
			}
		}
		if issue.Updated.IsZero() {
			if issue.Closed != nil {
				issue.Updated = *issue.Closed
			} else {
				issue.Updated = time.Now()
			}
		}

		var is = models.Issue{
			RepoID:      g.repo.ID,
			Repo:        g.repo,
			Index:       issue.Number,
			Title:       issue.Title,
			Content:     issue.Content,
			Ref:         issue.Ref,
			IsClosed:    issue.State == "closed",
			IsLocked:    issue.IsLocked,
			MilestoneID: milestoneID,
			Labels:      labels,
			CreatedUnix: timeutil.TimeStamp(issue.Created.Unix()),
			UpdatedUnix: timeutil.TimeStamp(issue.Updated.Unix()),
		}

		userid, ok := g.userMap[issue.PosterID]
		tp := g.gitServiceType.Name()
		if !ok && tp != "" {
			var err error
			userid, err = user_model.GetUserIDByExternalUserID(tp, fmt.Sprintf("%v", issue.PosterID))
			if err != nil {
				log.Error("GetUserIDByExternalUserID: %v", err)
			}
			if userid > 0 {
				g.userMap[issue.PosterID] = userid
			}
		}

		if userid > 0 {
			is.PosterID = userid
		} else {
			is.PosterID = g.doer.ID
			is.OriginalAuthor = issue.PosterName
			is.OriginalAuthorID = issue.PosterID
		}

		if issue.Closed != nil {
			is.ClosedUnix = timeutil.TimeStamp(issue.Closed.Unix())
		}
		// add reactions
		for _, reaction := range issue.Reactions {
			userid, ok := g.userMap[reaction.UserID]
			if !ok && tp != "" {
				var err error
				userid, err = user_model.GetUserIDByExternalUserID(tp, fmt.Sprintf("%v", reaction.UserID))
				if err != nil {
					log.Error("GetUserIDByExternalUserID: %v", err)
				}
				if userid > 0 {
					g.userMap[reaction.UserID] = userid
				}
			}
			var res = models.Reaction{
				Type:        reaction.Content,
				CreatedUnix: timeutil.TimeStampNow(),
			}
			if userid > 0 {
				res.UserID = userid
			} else {
				res.UserID = g.doer.ID
				res.OriginalAuthorID = reaction.UserID
				res.OriginalAuthor = reaction.UserName
			}
			is.Reactions = append(is.Reactions, &res)
		}
		iss = append(iss, &is)
	}

	if len(iss) > 0 {
		if err := models.InsertIssues(iss...); err != nil {
			return err
		}

		for _, is := range iss {
			g.issues.Store(is.Index, is)
		}
	}

	return nil
}

// CreateComments creates comments of issues
func (g *GiteaLocalUploader) CreateComments(comments ...*base.Comment) error {
	var cms = make([]*models.Comment, 0, len(comments))
	for _, comment := range comments {
		var issue *models.Issue
		issueInter, ok := g.issues.Load(comment.IssueIndex)
		if !ok {
			var err error
			issue, err = models.GetIssueByIndex(g.repo.ID, comment.IssueIndex)
			if err != nil {
				return err
			}
			g.issues.Store(comment.IssueIndex, issue)
		} else {
			issue = issueInter.(*models.Issue)
		}

		userid, ok := g.userMap[comment.PosterID]
		tp := g.gitServiceType.Name()
		if !ok && tp != "" {
			var err error
			userid, err = user_model.GetUserIDByExternalUserID(tp, fmt.Sprintf("%v", comment.PosterID))
			if err != nil {
				log.Error("GetUserIDByExternalUserID: %v", err)
			}
			if userid > 0 {
				g.userMap[comment.PosterID] = userid
			}
		}

		if comment.Created.IsZero() {
			comment.Created = time.Unix(int64(issue.CreatedUnix), 0)
		}
		if comment.Updated.IsZero() {
			comment.Updated = comment.Created
		}

		cm := models.Comment{
			IssueID:     issue.ID,
			Type:        models.CommentTypeComment,
			Content:     comment.Content,
			CreatedUnix: timeutil.TimeStamp(comment.Created.Unix()),
			UpdatedUnix: timeutil.TimeStamp(comment.Updated.Unix()),
		}

		if userid > 0 {
			cm.PosterID = userid
		} else {
			cm.PosterID = g.doer.ID
			cm.OriginalAuthor = comment.PosterName
			cm.OriginalAuthorID = comment.PosterID
		}

		// add reactions
		for _, reaction := range comment.Reactions {
			userid, ok := g.userMap[reaction.UserID]
			if !ok && tp != "" {
				var err error
				userid, err = user_model.GetUserIDByExternalUserID(tp, fmt.Sprintf("%v", reaction.UserID))
				if err != nil {
					log.Error("GetUserIDByExternalUserID: %v", err)
				}
				if userid > 0 {
					g.userMap[reaction.UserID] = userid
				}
			}
			var res = models.Reaction{
				Type:        reaction.Content,
				CreatedUnix: timeutil.TimeStampNow(),
			}
			if userid > 0 {
				res.UserID = userid
			} else {
				res.UserID = g.doer.ID
				res.OriginalAuthorID = reaction.UserID
				res.OriginalAuthor = reaction.UserName
			}
			cm.Reactions = append(cm.Reactions, &res)
		}

		cms = append(cms, &cm)
	}

	if len(cms) == 0 {
		return nil
	}
	return models.InsertIssueComments(cms)
}

// CreatePullRequests creates pull requests
func (g *GiteaLocalUploader) CreatePullRequests(prs ...*base.PullRequest) error {
	var gprs = make([]*models.PullRequest, 0, len(prs))
	for _, pr := range prs {
		gpr, err := g.newPullRequest(pr)
		if err != nil {
			return err
		}

		userid, ok := g.userMap[pr.PosterID]
		tp := g.gitServiceType.Name()
		if !ok && tp != "" {
			var err error
			userid, err = user_model.GetUserIDByExternalUserID(tp, fmt.Sprintf("%v", pr.PosterID))
			if err != nil {
				log.Error("GetUserIDByExternalUserID: %v", err)
			}
			if userid > 0 {
				g.userMap[pr.PosterID] = userid
			}
		}

		if userid > 0 {
			gpr.Issue.PosterID = userid
		} else {
			gpr.Issue.PosterID = g.doer.ID
			gpr.Issue.OriginalAuthor = pr.PosterName
			gpr.Issue.OriginalAuthorID = pr.PosterID
		}

		gprs = append(gprs, gpr)
	}
	if err := models.InsertPullRequests(gprs...); err != nil {
		return err
	}
	for _, pr := range gprs {
		g.issues.Store(pr.Issue.Index, pr.Issue)
		pull.AddToTaskQueue(pr)
	}
	return nil
}

func (g *GiteaLocalUploader) newPullRequest(pr *base.PullRequest) (*models.PullRequest, error) {
	var labels []*models.Label
	for _, label := range pr.Labels {
		lb, ok := g.labels.Load(label.Name)
		if ok {
			labels = append(labels, lb.(*models.Label))
		}
	}

	var milestoneID int64
	if pr.Milestone != "" {
		milestone, ok := g.milestones.Load(pr.Milestone)
		if ok {
			milestoneID = milestone.(int64)
		}
	}

	// download patch file
	err := func() error {
		if pr.PatchURL == "" {
			return nil
		}
		// pr.PatchURL maybe a local file
		ret, err := uri.Open(pr.PatchURL)
		if err != nil {
			return err
		}
		defer ret.Close()
		pullDir := filepath.Join(g.repo.RepoPath(), "pulls")
		if err = os.MkdirAll(pullDir, os.ModePerm); err != nil {
			return err
		}
		f, err := os.Create(filepath.Join(pullDir, fmt.Sprintf("%d.patch", pr.Number)))
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(f, ret)
		return err
	}()
	if err != nil {
		return nil, err
	}

	// set head information
	pullHead := filepath.Join(g.repo.RepoPath(), "refs", "pull", fmt.Sprintf("%d", pr.Number))
	if err := os.MkdirAll(pullHead, os.ModePerm); err != nil {
		return nil, err
	}
	p, err := os.Create(filepath.Join(pullHead, "head"))
	if err != nil {
		return nil, err
	}
	_, err = p.WriteString(pr.Head.SHA)
	p.Close()
	if err != nil {
		return nil, err
	}

	var head = "unknown repository"
	if pr.IsForkPullRequest() && pr.State != "closed" {
		if pr.Head.OwnerName != "" {
			remote := pr.Head.OwnerName
			_, ok := g.prHeadCache[remote]
			if !ok {
				// git remote add
				err := g.gitRepo.AddRemote(remote, pr.Head.CloneURL, true)
				if err != nil {
					log.Error("AddRemote failed: %s", err)
				} else {
					g.prHeadCache[remote] = struct{}{}
					ok = true
				}
			}

			if ok {
				_, err = git.NewCommand("fetch", remote, pr.Head.Ref).RunInDir(g.repo.RepoPath())
				if err != nil {
					log.Error("Fetch branch from %s failed: %v", pr.Head.CloneURL, err)
				} else {
					headBranch := filepath.Join(g.repo.RepoPath(), "refs", "heads", pr.Head.OwnerName, pr.Head.Ref)
					if err := os.MkdirAll(filepath.Dir(headBranch), os.ModePerm); err != nil {
						return nil, err
					}
					b, err := os.Create(headBranch)
					if err != nil {
						return nil, err
					}
					_, err = b.WriteString(pr.Head.SHA)
					b.Close()
					if err != nil {
						return nil, err
					}
					head = pr.Head.OwnerName + "/" + pr.Head.Ref
				}
			}
		}
	} else {
		head = pr.Head.Ref
		// Ensure the closed PR SHA still points to an existing ref
		_, err = git.NewCommand("rev-list", "--quiet", "-1", pr.Head.SHA).RunInDir(g.repo.RepoPath())
		if err != nil {
			if pr.Head.SHA != "" {
				// Git update-ref remove bad references with a relative path
				log.Warn("Deprecated local head, removing : %v", pr.Head.SHA)
				err = g.gitRepo.RemoveReference(pr.GetGitRefName())
			} else {
				// The SHA is empty, remove the head file
				log.Warn("Empty reference, removing : %v", pullHead)
				err = os.Remove(filepath.Join(pullHead, "head"))
			}
			if err != nil {
				log.Error("Cannot remove local head ref, %v", err)
			}
		}
	}

	if pr.Created.IsZero() {
		if pr.Closed != nil {
			pr.Created = *pr.Closed
		} else if pr.MergedTime != nil {
			pr.Created = *pr.MergedTime
		} else {
			pr.Created = time.Now()
		}
	}
	if pr.Updated.IsZero() {
		pr.Updated = pr.Created
	}

	var issue = models.Issue{
		RepoID:      g.repo.ID,
		Repo:        g.repo,
		Title:       pr.Title,
		Index:       pr.Number,
		Content:     pr.Content,
		MilestoneID: milestoneID,
		IsPull:      true,
		IsClosed:    pr.State == "closed",
		IsLocked:    pr.IsLocked,
		Labels:      labels,
		CreatedUnix: timeutil.TimeStamp(pr.Created.Unix()),
		UpdatedUnix: timeutil.TimeStamp(pr.Updated.Unix()),
	}

	tp := g.gitServiceType.Name()

	userid, ok := g.userMap[pr.PosterID]
	if !ok && tp != "" {
		var err error
		userid, err = user_model.GetUserIDByExternalUserID(tp, fmt.Sprintf("%v", pr.PosterID))
		if err != nil {
			log.Error("GetUserIDByExternalUserID: %v", err)
		}
		if userid > 0 {
			g.userMap[pr.PosterID] = userid
		}
	}

	if userid > 0 {
		issue.PosterID = userid
	} else {
		issue.PosterID = g.doer.ID
		issue.OriginalAuthor = pr.PosterName
		issue.OriginalAuthorID = pr.PosterID
	}

	// add reactions
	for _, reaction := range pr.Reactions {
		userid, ok := g.userMap[reaction.UserID]
		if !ok && tp != "" {
			var err error
			userid, err = user_model.GetUserIDByExternalUserID(tp, fmt.Sprintf("%v", reaction.UserID))
			if err != nil {
				log.Error("GetUserIDByExternalUserID: %v", err)
			}
			if userid > 0 {
				g.userMap[reaction.UserID] = userid
			}
		}
		var res = models.Reaction{
			Type:        reaction.Content,
			CreatedUnix: timeutil.TimeStampNow(),
		}
		if userid > 0 {
			res.UserID = userid
		} else {
			res.UserID = g.doer.ID
			res.OriginalAuthorID = reaction.UserID
			res.OriginalAuthor = reaction.UserName
		}
		issue.Reactions = append(issue.Reactions, &res)
	}

	var pullRequest = models.PullRequest{
		HeadRepoID: g.repo.ID,
		HeadBranch: head,
		BaseRepoID: g.repo.ID,
		BaseBranch: pr.Base.Ref,
		MergeBase:  pr.Base.SHA,
		Index:      pr.Number,
		HasMerged:  pr.Merged,

		Issue: &issue,
	}

	if pullRequest.Issue.IsClosed && pr.Closed != nil {
		pullRequest.Issue.ClosedUnix = timeutil.TimeStamp(pr.Closed.Unix())
	}
	if pullRequest.HasMerged && pr.MergedTime != nil {
		pullRequest.MergedUnix = timeutil.TimeStamp(pr.MergedTime.Unix())
		pullRequest.MergedCommitID = pr.MergeCommitSHA
		pullRequest.MergerID = g.doer.ID
	}

	// TODO: assignees

	return &pullRequest, nil
}

func convertReviewState(state string) models.ReviewType {
	switch state {
	case base.ReviewStatePending:
		return models.ReviewTypePending
	case base.ReviewStateApproved:
		return models.ReviewTypeApprove
	case base.ReviewStateChangesRequested:
		return models.ReviewTypeReject
	case base.ReviewStateCommented:
		return models.ReviewTypeComment
	default:
		return models.ReviewTypePending
	}
}

// CreateReviews create pull request reviews
func (g *GiteaLocalUploader) CreateReviews(reviews ...*base.Review) error {
	var cms = make([]*models.Review, 0, len(reviews))
	for _, review := range reviews {
		var issue *models.Issue
		issueInter, ok := g.issues.Load(review.IssueIndex)
		if !ok {
			var err error
			issue, err = models.GetIssueByIndex(g.repo.ID, review.IssueIndex)
			if err != nil {
				return err
			}
			g.issues.Store(review.IssueIndex, issue)
		} else {
			issue = issueInter.(*models.Issue)
		}

		userid, ok := g.userMap[review.ReviewerID]
		tp := g.gitServiceType.Name()
		if !ok && tp != "" {
			var err error
			userid, err = user_model.GetUserIDByExternalUserID(tp, fmt.Sprintf("%v", review.ReviewerID))
			if err != nil {
				log.Error("GetUserIDByExternalUserID: %v", err)
			}
			if userid > 0 {
				g.userMap[review.ReviewerID] = userid
			}
		}

		if review.CreatedAt.IsZero() {
			review.CreatedAt = time.Unix(int64(issue.CreatedUnix), 0)
		}

		var cm = models.Review{
			Type:        convertReviewState(review.State),
			IssueID:     issue.ID,
			Content:     review.Content,
			Official:    review.Official,
			CreatedUnix: timeutil.TimeStamp(review.CreatedAt.Unix()),
			UpdatedUnix: timeutil.TimeStamp(review.CreatedAt.Unix()),
		}

		if userid > 0 {
			cm.ReviewerID = userid
		} else {
			cm.ReviewerID = g.doer.ID
			cm.OriginalAuthor = review.ReviewerName
			cm.OriginalAuthorID = review.ReviewerID
		}

		// get pr
		pr, ok := g.prCache[issue.ID]
		if !ok {
			var err error
			pr, err = models.GetPullRequestByIssueIDWithNoAttributes(issue.ID)
			if err != nil {
				return err
			}
			g.prCache[issue.ID] = pr
		}

		for _, comment := range review.Comments {
			line := comment.Line
			if line != 0 {
				comment.Position = 1
			} else {
				_, _, line, _ = git.ParseDiffHunkString(comment.DiffHunk)
			}
			headCommitID, err := g.gitRepo.GetRefCommitID(pr.GetGitRefName())
			if err != nil {
				log.Warn("GetRefCommitID[%s]: %v, the review comment will be ignored", pr.GetGitRefName(), err)
				continue
			}

			var patch string
			reader, writer := io.Pipe()
			defer func() {
				_ = reader.Close()
				_ = writer.Close()
			}()
			go func() {
				if err := git.GetRepoRawDiffForFile(g.gitRepo, pr.MergeBase, headCommitID, git.RawDiffNormal, comment.TreePath, writer); err != nil {
					// We should ignore the error since the commit maybe removed when force push to the pull request
					log.Warn("GetRepoRawDiffForFile failed when migrating [%s, %s, %s, %s]: %v", g.gitRepo.Path, pr.MergeBase, headCommitID, comment.TreePath, err)
				}
				_ = writer.Close()
			}()

			patch, _ = git.CutDiffAroundLine(reader, int64((&models.Comment{Line: int64(line + comment.Position - 1)}).UnsignedLine()), line < 0, setting.UI.CodeCommentLines)

			if comment.CreatedAt.IsZero() {
				comment.CreatedAt = review.CreatedAt
			}
			if comment.UpdatedAt.IsZero() {
				comment.UpdatedAt = comment.CreatedAt
			}

			var c = models.Comment{
				Type:        models.CommentTypeCode,
				PosterID:    comment.PosterID,
				IssueID:     issue.ID,
				Content:     comment.Content,
				Line:        int64(line + comment.Position - 1),
				TreePath:    comment.TreePath,
				CommitSHA:   comment.CommitID,
				Patch:       patch,
				CreatedUnix: timeutil.TimeStamp(comment.CreatedAt.Unix()),
				UpdatedUnix: timeutil.TimeStamp(comment.UpdatedAt.Unix()),
			}

			if userid > 0 {
				c.PosterID = userid
			} else {
				c.PosterID = g.doer.ID
				c.OriginalAuthor = review.ReviewerName
				c.OriginalAuthorID = review.ReviewerID
			}

			cm.Comments = append(cm.Comments, &c)
		}

		cms = append(cms, &cm)
	}

	return models.InsertReviews(cms)
}

// Rollback when migrating failed, this will rollback all the changes.
func (g *GiteaLocalUploader) Rollback() error {
	if g.repo != nil && g.repo.ID > 0 {
		g.gitRepo.Close()
		if err := models.DeleteRepository(g.doer, g.repo.OwnerID, g.repo.ID); err != nil {
			return err
		}
	}
	return nil
}

// Finish when migrating success, this will do some status update things.
func (g *GiteaLocalUploader) Finish() error {
	if g.repo == nil || g.repo.ID <= 0 {
		return ErrRepoNotCreated
	}

	// update issue_index
	if err := models.RecalculateIssueIndexForRepo(g.repo.ID); err != nil {
		return err
	}

	g.repo.Status = repo_model.RepositoryReady
	return repo_model.UpdateRepositoryCols(g.repo, "status")
}
