// Copyright 2019 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package repository

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"code.gitea.io/gitea/models"
	"code.gitea.io/gitea/models/db"
	repo_model "code.gitea.io/gitea/models/repo"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/lfs"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/migration"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/util"

	"gopkg.in/ini.v1"
)

/*
	GitHub, GitLab, Gogs: *.wiki.git
	BitBucket: *.git/wiki
*/
var commonWikiURLSuffixes = []string{".wiki.git", ".git/wiki"}

// WikiRemoteURL returns accessible repository URL for wiki if exists.
// Otherwise, it returns an empty string.
func WikiRemoteURL(remote, username, password string) string {
	remote = strings.TrimSuffix(remote, ".git")
	credentialsArgs := []string{}
	if len(username) > 0 {
		if len(password) == 0 {
			credentialsArgs = append(credentialsArgs,
				"-c",
				fmt.Sprintf("credential.helper=%s -c %s credential-helper --username %s", setting.AppPath, setting.CustomConf, username),
			)
		} else {
			credentialsArgs = append(credentialsArgs,
				"-c",
				fmt.Sprintf("credential.helper=%s -c %s credential-helper --username %s --password %s", setting.AppPath, setting.CustomConf, username, password),
			)
		}
	}
	for _, suffix := range commonWikiURLSuffixes {
		wikiURL := remote + suffix
		if git.IsRepoURLAccessibleWithArgs(wikiURL, credentialsArgs) {
			return wikiURL
		}
	}
	return ""
}

// CreateCredentialsHelper creates git credentials helper git command option
func CreateCredentialsHelper(username, password string) []string {
	credentialsArgs := []string{}
	if len(username) > 0 {
		credentialsArgs = append(credentialsArgs,
			"-c",
			fmt.Sprintf("credential.helper=%s -c %s credential-helper --username %s --password %s", setting.AppPath, setting.CustomConf, username, password),
		)
	}
	return credentialsArgs
}

// MigrateRepositoryGitData starts migrating git related data after created migrating repository
func MigrateRepositoryGitData(ctx context.Context, u *user_model.User,
	repo *repo_model.Repository, opts migration.MigrateOptions,
	httpTransport *http.Transport,
) (*repo_model.Repository, error) {
	repoPath := repo_model.RepoPath(u.Name, opts.RepoName)

	if u.IsOrganization() {
		t, err := models.OrgFromUser(u).GetOwnerTeam()
		if err != nil {
			return nil, err
		}
		repo.NumWatches = t.NumMembers
	} else {
		repo.NumWatches = 1
	}

	migrateTimeout := time.Duration(setting.Git.Timeout.Migrate) * time.Second

	var err error
	if err = util.RemoveAll(repoPath); err != nil {
		return repo, fmt.Errorf("Failed to remove %s: %v", repoPath, err)
	}

	gitArgs := make([]string, len(git.GlobalCommandArgs))
	copy(gitArgs, git.GlobalCommandArgs)
	credentialsArgs := CreateCredentialsHelper(opts.AuthUsername, opts.AuthPassword)
	gitArgs = append(gitArgs, credentialsArgs...)

	if err = git.CloneWithArgs(ctx, opts.CloneAddr, repoPath, gitArgs, git.CloneRepoOptions{
		Mirror:  true,
		Quiet:   true,
		Timeout: migrateTimeout,
	}); err != nil {
		return repo, fmt.Errorf("Clone: %v", err)
	}

	if opts.Wiki {
		wikiPath := repo_model.WikiPath(u.Name, opts.RepoName)
		wikiRemotePath := WikiRemoteURL(opts.CloneAddr, opts.AuthUsername, opts.AuthPassword)
		if len(wikiRemotePath) > 0 {
			if err := util.RemoveAll(wikiPath); err != nil {
				return repo, fmt.Errorf("Failed to remove %s: %v", wikiPath, err)
			}

			if err = git.CloneWithArgs(ctx, wikiRemotePath, wikiPath, gitArgs, git.CloneRepoOptions{
				Mirror:  true,
				Quiet:   true,
				Timeout: migrateTimeout,
				Branch:  "master",
			}); err != nil {
				log.Warn("Clone wiki: %v", err)
				if err := util.RemoveAll(wikiPath); err != nil {
					return repo, fmt.Errorf("Failed to remove %s: %v", wikiPath, err)
				}
			}
		}
	}

	if repo.OwnerID == u.ID {
		repo.Owner = u
	}

	if err = models.CheckDaemonExportOK(ctx, repo); err != nil {
		return repo, fmt.Errorf("checkDaemonExportOK: %v", err)
	}

	if stdout, err := git.NewCommandContext(ctx, "update-server-info").
		SetDescription(fmt.Sprintf("MigrateRepositoryGitData(git update-server-info): %s", repoPath)).
		RunInDir(repoPath); err != nil {
		log.Error("MigrateRepositoryGitData(git update-server-info) in %v: Stdout: %s\nError: %v", repo, stdout, err)
		return repo, fmt.Errorf("error in MigrateRepositoryGitData(git update-server-info): %v", err)
	}

	gitRepo, err := git.OpenRepository(repoPath)
	if err != nil {
		return repo, fmt.Errorf("OpenRepository: %v", err)
	}
	defer gitRepo.Close()

	repo.IsEmpty, err = gitRepo.IsEmpty()
	if err != nil {
		return repo, fmt.Errorf("git.IsEmpty: %v", err)
	}

	if !repo.IsEmpty {
		if len(repo.DefaultBranch) == 0 {
			// Try to get HEAD branch and set it as default branch.
			headBranch, err := gitRepo.GetHEADBranch()
			if err != nil {
				return repo, fmt.Errorf("GetHEADBranch: %v", err)
			}
			if headBranch != nil {
				repo.DefaultBranch = headBranch.Name
			}
		}

		if !opts.Releases {
			if err = SyncReleasesWithTags(repo, gitRepo); err != nil {
				log.Error("Failed to synchronize tags to releases for repository: %v", err)
			}
		}

		if opts.LFS {
			lfsAddr := opts.CloneAddr
			if lfsURL, err := url.Parse(lfsAddr); err == nil {
				if len(opts.AuthUsername) > 0 {
					if len(opts.AuthPassword) > 0 {
						lfsURL.User = url.UserPassword(opts.AuthUsername, opts.AuthPassword)
					} else {
						lfsURL.User = url.User(opts.AuthUsername)
					}
				}
				lfsAddr = lfsURL.String()
			}
			endpoint := lfs.DetermineEndpoint(lfsAddr, opts.LFSEndpoint)
			lfsClient := lfs.NewClient(endpoint, httpTransport)
			if err = StoreMissingLfsObjectsInRepository(ctx, repo, gitRepo, lfsClient); err != nil {
				log.Error("Failed to store missing LFS objects for repository: %v", err)
			}
		}
	}

	if err = models.UpdateRepoSize(db.DefaultContext, repo); err != nil {
		log.Error("Failed to update size for repository: %v", err)
	}

	if opts.Mirror {
		mirrorModel := repo_model.Mirror{
			RepoID:         repo.ID,
			Interval:       setting.Mirror.DefaultInterval,
			EnablePrune:    true,
			NextUpdateUnix: timeutil.TimeStampNow().AddDuration(setting.Mirror.DefaultInterval),
			MirrorUsername: opts.AuthUsername,
			MirrorPassword: opts.AuthPassword,
			LFS:            opts.LFS,
		}
		if opts.LFS {
			mirrorModel.LFSEndpoint = opts.LFSEndpoint
		}

		if opts.MirrorInterval != "" {
			parsedInterval, err := time.ParseDuration(opts.MirrorInterval)
			if err != nil {
				log.Error("Failed to set Interval: %v", err)
				return repo, err
			}
			if parsedInterval == 0 {
				mirrorModel.Interval = 0
				mirrorModel.NextUpdateUnix = 0
			} else if parsedInterval < setting.Mirror.MinInterval {
				err := fmt.Errorf("Interval %s is set below Minimum Interval of %s", parsedInterval, setting.Mirror.MinInterval)
				log.Error("Interval: %s is too frequent", opts.MirrorInterval)
				return repo, err
			} else {
				mirrorModel.Interval = parsedInterval
				mirrorModel.NextUpdateUnix = timeutil.TimeStampNow().AddDuration(parsedInterval)
			}
		}

		if err = repo_model.InsertMirror(&mirrorModel); err != nil {
			return repo, fmt.Errorf("InsertOne: %v", err)
		}

		repo.IsMirror = true
		err = models.UpdateRepository(repo, false)
	} else {
		repo, err = CleanUpMigrateInfo(repo)
	}

	return repo, err
}

// cleanUpMigrateGitConfig removes mirror info which prevents "push --all".
// This also removes possible user credentials.
func cleanUpMigrateGitConfig(configPath string) error {
	cfg, err := ini.Load(configPath)
	if err != nil {
		return fmt.Errorf("open config file: %v", err)
	}
	cfg.DeleteSection("remote \"origin\"")
	if err = cfg.SaveToIndent(configPath, "\t"); err != nil {
		return fmt.Errorf("save config file: %v", err)
	}
	return nil
}

// CleanUpMigrateInfo finishes migrating repository and/or wiki with things that don't need to be done for mirrors.
func CleanUpMigrateInfo(repo *repo_model.Repository) (*repo_model.Repository, error) {
	repoPath := repo.RepoPath()
	if err := createDelegateHooks(repoPath); err != nil {
		return repo, fmt.Errorf("createDelegateHooks: %v", err)
	}
	if repo.HasWiki() {
		if err := createDelegateHooks(repo.WikiPath()); err != nil {
			return repo, fmt.Errorf("createDelegateHooks.(wiki): %v", err)
		}
	}

	_, err := git.NewCommand("remote", "rm", "origin").RunInDir(repoPath)
	if err != nil && !strings.HasPrefix(err.Error(), "exit status 128 - fatal: No such remote ") {
		return repo, fmt.Errorf("CleanUpMigrateInfo: %v", err)
	}

	if repo.HasWiki() {
		if err := cleanUpMigrateGitConfig(path.Join(repo.WikiPath(), "config")); err != nil {
			return repo, fmt.Errorf("cleanUpMigrateGitConfig (wiki): %v", err)
		}
	}

	return repo, models.UpdateRepository(repo, false)
}

// SyncReleasesWithTags synchronizes release table with repository tags
func SyncReleasesWithTags(repo *repo_model.Repository, gitRepo *git.Repository) error {
	existingRelTags := make(map[string]struct{})
	opts := models.FindReleasesOptions{
		IncludeDrafts: true,
		IncludeTags:   true,
		ListOptions:   db.ListOptions{PageSize: 50},
	}
	for page := 1; ; page++ {
		opts.Page = page
		rels, err := models.GetReleasesByRepoID(repo.ID, opts)
		if err != nil {
			return fmt.Errorf("GetReleasesByRepoID: %v", err)
		}
		if len(rels) == 0 {
			break
		}
		for _, rel := range rels {
			if rel.IsDraft {
				continue
			}
			commitID, err := gitRepo.GetTagCommitID(rel.TagName)
			if err != nil && !git.IsErrNotExist(err) {
				return fmt.Errorf("GetTagCommitID: %s: %v", rel.TagName, err)
			}
			if git.IsErrNotExist(err) || commitID != rel.Sha1 {
				if err := models.PushUpdateDeleteTag(repo, rel.TagName); err != nil {
					return fmt.Errorf("PushUpdateDeleteTag: %s: %v", rel.TagName, err)
				}
			} else {
				existingRelTags[strings.ToLower(rel.TagName)] = struct{}{}
			}
		}
	}
	tags, err := gitRepo.GetTags(0, 0)
	if err != nil {
		return fmt.Errorf("GetTags: %v", err)
	}
	for _, tagName := range tags {
		if _, ok := existingRelTags[strings.ToLower(tagName)]; !ok {
			if err := PushUpdateAddTag(repo, gitRepo, tagName); err != nil {
				return fmt.Errorf("pushUpdateAddTag: %v", err)
			}
		}
	}
	return nil
}

// PushUpdateAddTag must be called for any push actions to add tag
func PushUpdateAddTag(repo *repo_model.Repository, gitRepo *git.Repository, tagName string) error {
	tag, err := gitRepo.GetTag(tagName)
	if err != nil {
		return fmt.Errorf("GetTag: %v", err)
	}
	commit, err := tag.Commit(gitRepo)
	if err != nil {
		return fmt.Errorf("Commit: %v", err)
	}

	sig := tag.Tagger
	if sig == nil {
		sig = commit.Author
	}
	if sig == nil {
		sig = commit.Committer
	}

	var author *user_model.User
	var createdAt = time.Unix(1, 0)

	if sig != nil {
		author, err = user_model.GetUserByEmail(sig.Email)
		if err != nil && !user_model.IsErrUserNotExist(err) {
			return fmt.Errorf("GetUserByEmail: %v", err)
		}
		createdAt = sig.When
	}

	commitsCount, err := commit.CommitsCount()
	if err != nil {
		return fmt.Errorf("CommitsCount: %v", err)
	}

	var rel = models.Release{
		RepoID:       repo.ID,
		TagName:      tagName,
		LowerTagName: strings.ToLower(tagName),
		Sha1:         commit.ID.String(),
		NumCommits:   commitsCount,
		CreatedUnix:  timeutil.TimeStamp(createdAt.Unix()),
		IsTag:        true,
	}
	if author != nil {
		rel.PublisherID = author.ID
	}

	return models.SaveOrUpdateTag(repo, &rel)
}

// StoreMissingLfsObjectsInRepository downloads missing LFS objects
func StoreMissingLfsObjectsInRepository(ctx context.Context, repo *repo_model.Repository, gitRepo *git.Repository, lfsClient lfs.Client) error {
	contentStore := lfs.NewContentStore()

	pointerChan := make(chan lfs.PointerBlob)
	errChan := make(chan error, 1)
	go lfs.SearchPointerBlobs(ctx, gitRepo, pointerChan, errChan)

	downloadObjects := func(pointers []lfs.Pointer) error {
		err := lfsClient.Download(ctx, pointers, func(p lfs.Pointer, content io.ReadCloser, objectError error) error {
			if objectError != nil {
				return objectError
			}

			defer content.Close()

			_, err := models.NewLFSMetaObject(&models.LFSMetaObject{Pointer: p, RepositoryID: repo.ID})
			if err != nil {
				log.Error("Error creating LFS meta object %v: %v", p, err)
				return err
			}

			if err := contentStore.Put(p, content); err != nil {
				log.Error("Error storing content for LFS meta object %v: %v", p, err)
				if _, err2 := models.RemoveLFSMetaObjectByOid(repo.ID, p.Oid); err2 != nil {
					log.Error("Error removing LFS meta object %v: %v", p, err2)
				}
				return err
			}
			return nil
		})
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
		}
		return err
	}

	var batch []lfs.Pointer
	for pointerBlob := range pointerChan {
		meta, err := models.GetLFSMetaObjectByOid(repo.ID, pointerBlob.Oid)
		if err != nil && err != models.ErrLFSObjectNotExist {
			log.Error("Error querying LFS meta object %v: %v", pointerBlob.Pointer, err)
			return err
		}
		if meta != nil {
			log.Trace("Skipping unknown LFS meta object %v", pointerBlob.Pointer)
			continue
		}

		log.Trace("LFS object %v not present in repository %s", pointerBlob.Pointer, repo.FullName())

		exist, err := contentStore.Exists(pointerBlob.Pointer)
		if err != nil {
			log.Error("Error checking if LFS object %v exists: %v", pointerBlob.Pointer, err)
			return err
		}

		if exist {
			log.Trace("LFS object %v already present; creating meta object", pointerBlob.Pointer)
			_, err := models.NewLFSMetaObject(&models.LFSMetaObject{Pointer: pointerBlob.Pointer, RepositoryID: repo.ID})
			if err != nil {
				log.Error("Error creating LFS meta object %v: %v", pointerBlob.Pointer, err)
				return err
			}
		} else {
			if setting.LFS.MaxFileSize > 0 && pointerBlob.Size > setting.LFS.MaxFileSize {
				log.Info("LFS object %v download denied because of LFS_MAX_FILE_SIZE=%d < size %d", pointerBlob.Pointer, setting.LFS.MaxFileSize, pointerBlob.Size)
				continue
			}

			batch = append(batch, pointerBlob.Pointer)
			if len(batch) >= lfsClient.BatchSize() {
				if err := downloadObjects(batch); err != nil {
					return err
				}
				batch = nil
			}
		}
	}
	if len(batch) > 0 {
		if err := downloadObjects(batch); err != nil {
			return err
		}
	}

	err, has := <-errChan
	if has {
		log.Error("Error enumerating LFS objects for repository: %v", err)
		return err
	}

	return nil
}
