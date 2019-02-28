// Copyright 2019 Gitea. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"code.gitea.io/gitea/models"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/migrations"
	"code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/util"
)

func handleCreateError(owner *models.User, err error, name string) error {
	switch {
	case models.IsErrReachLimitOfRepo(err):
		return fmt.Errorf("You have already reached your limit of %d repositories.", owner.MaxCreationLimit())
	case models.IsErrRepoAlreadyExist(err):
		return errors.New("The repository name is already used.")
	case models.IsErrNameReserved(err):
		return fmt.Errorf("The repository name '%s' is reserved.", err.(models.ErrNameReserved).Name)
	case models.IsErrNamePatternNotAllowed(err):
		return fmt.Errorf("The pattern '%s' is not allowed in a repository name.", err.(models.ErrNamePatternNotAllowed).Pattern)
	default:
		return err
	}
}

func runMigrateTask(t *models.Task) error {
	var opts migrations.MigrateOptions
	err := t.DecodeConfig(&opts)
	if err != nil {
		return err
	}

	defer func() {
		if e := recover(); e != nil {
			var buf bytes.Buffer
			fmt.Fprintf(&buf, "Handler crashed with error: %v", e)

			for i := 1; ; i++ {
				_, file, line, ok := runtime.Caller(i)
				if !ok {
					break
				} else {
					fmt.Fprintf(&buf, "\n")
				}
				fmt.Fprintf(&buf, "%v:%v", file, line)
			}

			err = errors.New(buf.String())
		}

		if err != nil {
			t.EndTime = util.TimeStampNow()
			t.Status = structs.TaskStatusFailed
			t.Errors = err.Error()
			if err := t.UpdateCols("status", "errors", "end_time"); err != nil {
				log.Error("Task UpdateCols failed: %s", err.Error())
			} else if t.Repo != nil {
				if errDelete := models.DeleteRepository(t.Doer, t.OwnerID, t.Repo.ID); errDelete != nil {
					log.Error("DeleteRepository: %v", errDelete)
				}
			}
		} else {
			if err := models.FinishMigrateTask(t); err != nil {
				log.Error("Task UpdateCols failed: %s", err.Error())
			}
		}
	}()

	if err := t.LoadRepo(); err != nil {
		return err
	}

	if err := t.LoadDoer(); err != nil {
		return err
	}
	if err := t.LoadOwner(); err != nil {
		return err
	}
	t.StartTime = util.TimeStampNow()
	t.Status = structs.TaskStatusRunning
	if err := t.UpdateCols("start_time", "status"); err != nil {
		return err
	}

	repo, err := migrations.MigrateRepositoryGitData(t.Doer, t.Owner, t.Repo, opts)
	if err == nil {
		log.Trace("Repository migrated [%d]: %s/%s", repo.ID, t.Owner.Name, repo.Name)
		return nil
	}

	if models.IsErrRepoAlreadyExist(err) {
		return errors.New("The repository name is already used.")
	}

	// remoteAddr may contain credentials, so we sanitize it
	err = util.URLSanitizedError(err, opts.RemoteURL)
	if strings.Contains(err.Error(), "Authentication failed") ||
		strings.Contains(err.Error(), "could not read Username") {
		return fmt.Errorf("Authentication failed: %v", err.Error())
	} else if strings.Contains(err.Error(), "fatal:") {
		return fmt.Errorf("Migration failed: %v", err.Error())
	}

	return handleCreateError(t.Owner, err, "MigratePost")
}

// CreateMigrateTask creates a migrate task
func CreateMigrateTask(doer, u *models.User, opts migrations.MigrateOptions) (*models.Task, error) {
	bs, err := json.Marshal(&opts)
	if err != nil {
		return nil, err
	}

	var task = models.Task{
		DoerID:         doer.ID,
		OwnerID:        u.ID,
		Type:           structs.TaskTypeMigrateRepo,
		Status:         structs.TaskStatusQueue,
		PayloadContent: string(bs),
	}

	if err := models.CreateTask(&task); err != nil {
		return nil, err
	}

	repo, err := models.CreateRepository(doer, u, models.CreateRepoOptions{
		Name:        opts.Name,
		Description: opts.Description,
		IsPrivate:   opts.Private,
		IsMirror:    opts.Mirror,
		Status:      models.RepositoryCreating,
	})
	if err != nil {
		task.EndTime = util.TimeStampNow()
		task.Status = structs.TaskStatusFailed
		err2 := task.UpdateCols("end_time", "status")
		if err2 != nil {
			log.Error("UpdateCols Failed: %v", err2.Error())
		}
		return nil, err
	}

	task.RepoID = repo.ID
	if err = task.UpdateCols("repo_id"); err != nil {
		return nil, err
	}

	return &task, nil
}