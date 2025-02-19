// Copyright 2018 The Gitea Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package repo

import (
	"fmt"
	"net/http"

	"code.gitea.io/gitea/models"
	pull_model "code.gitea.io/gitea/models/pull"
	"code.gitea.io/gitea/modules/base"
	"code.gitea.io/gitea/modules/context"
	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/web"
	"code.gitea.io/gitea/services/forms"
	pull_service "code.gitea.io/gitea/services/pull"
)

const (
	tplConversation base.TplName = "repo/diff/conversation"
	tplNewComment   base.TplName = "repo/diff/new_comment"
)

// RenderNewCodeCommentForm will render the form for creating a new review comment
func RenderNewCodeCommentForm(ctx *context.Context) {
	issue := GetActionIssue(ctx)
	if !issue.IsPull {
		return
	}
	currentReview, err := models.GetCurrentReview(ctx.Doer, issue)
	if err != nil && !models.IsErrReviewNotExist(err) {
		ctx.ServerError("GetCurrentReview", err)
		return
	}
	ctx.Data["PageIsPullFiles"] = true
	ctx.Data["Issue"] = issue
	ctx.Data["CurrentReview"] = currentReview
	pullHeadCommitID, err := ctx.Repo.GitRepo.GetRefCommitID(issue.PullRequest.GetGitRefName())
	if err != nil {
		ctx.ServerError("GetRefCommitID", err)
		return
	}
	ctx.Data["AfterCommitID"] = pullHeadCommitID
	ctx.HTML(http.StatusOK, tplNewComment)
}

// CreateCodeComment will create a code comment including an pending review if required
func CreateCodeComment(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.CodeCommentForm)
	issue := GetActionIssue(ctx)
	if !issue.IsPull {
		return
	}
	if ctx.Written() {
		return
	}

	if ctx.HasError() {
		ctx.Flash.Error(ctx.Data["ErrorMsg"].(string))
		ctx.Redirect(fmt.Sprintf("%s/pulls/%d/files", ctx.Repo.RepoLink, issue.Index))
		return
	}

	signedLine := form.Line
	if form.Side == "previous" {
		signedLine *= -1
	}

	comment, err := pull_service.CreateCodeComment(ctx,
		ctx.Doer,
		ctx.Repo.GitRepo,
		issue,
		signedLine,
		form.Content,
		form.TreePath,
		form.IsReview,
		form.Reply,
		form.LatestCommitID,
	)
	if err != nil {
		ctx.ServerError("CreateCodeComment", err)
		return
	}

	if comment == nil {
		log.Trace("Comment not created: %-v #%d[%d]", ctx.Repo.Repository, issue.Index, issue.ID)
		ctx.Redirect(fmt.Sprintf("%s/pulls/%d/files", ctx.Repo.RepoLink, issue.Index))
		return
	}

	log.Trace("Comment created: %-v #%d[%d] Comment[%d]", ctx.Repo.Repository, issue.Index, issue.ID, comment.ID)

	if form.Origin == "diff" {
		renderConversation(ctx, comment)
		return
	}
	ctx.Redirect(comment.HTMLURL())
}

// UpdateResolveConversation add or remove an Conversation resolved mark
func UpdateResolveConversation(ctx *context.Context) {
	origin := ctx.FormString("origin")
	action := ctx.FormString("action")
	commentID := ctx.FormInt64("comment_id")

	comment, err := models.GetCommentByID(commentID)
	if err != nil {
		ctx.ServerError("GetIssueByID", err)
		return
	}

	if err = comment.LoadIssue(); err != nil {
		ctx.ServerError("comment.LoadIssue", err)
		return
	}

	var permResult bool
	if permResult, err = models.CanMarkConversation(comment.Issue, ctx.Doer); err != nil {
		ctx.ServerError("CanMarkConversation", err)
		return
	}
	if !permResult {
		ctx.Error(http.StatusForbidden)
		return
	}

	if !comment.Issue.IsPull {
		ctx.Error(http.StatusBadRequest)
		return
	}

	if action == "Resolve" || action == "UnResolve" {
		err = models.MarkConversation(comment, ctx.Doer, action == "Resolve")
		if err != nil {
			ctx.ServerError("MarkConversation", err)
			return
		}
	} else {
		ctx.Error(http.StatusBadRequest)
		return
	}

	if origin == "diff" {
		renderConversation(ctx, comment)
		return
	}
	ctx.JSON(http.StatusOK, map[string]interface{}{
		"ok": true,
	})
}

func renderConversation(ctx *context.Context, comment *models.Comment) {
	comments, err := models.FetchCodeCommentsByLine(ctx, comment.Issue, ctx.Doer, comment.TreePath, comment.Line)
	if err != nil {
		ctx.ServerError("FetchCodeCommentsByLine", err)
		return
	}
	ctx.Data["PageIsPullFiles"] = true
	ctx.Data["comments"] = comments
	ctx.Data["CanMarkConversation"] = true
	ctx.Data["Issue"] = comment.Issue
	if err = comment.Issue.LoadPullRequest(); err != nil {
		ctx.ServerError("comment.Issue.LoadPullRequest", err)
		return
	}
	pullHeadCommitID, err := ctx.Repo.GitRepo.GetRefCommitID(comment.Issue.PullRequest.GetGitRefName())
	if err != nil {
		ctx.ServerError("GetRefCommitID", err)
		return
	}
	ctx.Data["AfterCommitID"] = pullHeadCommitID
	ctx.HTML(http.StatusOK, tplConversation)
}

// SubmitReview creates a review out of the existing pending review or creates a new one if no pending review exist
func SubmitReview(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.SubmitReviewForm)
	issue := GetActionIssue(ctx)
	if !issue.IsPull {
		return
	}
	if ctx.Written() {
		return
	}
	if ctx.HasError() {
		ctx.Flash.Error(ctx.Data["ErrorMsg"].(string))
		ctx.Redirect(fmt.Sprintf("%s/pulls/%d/files", ctx.Repo.RepoLink, issue.Index))
		return
	}

	reviewType := form.ReviewType()
	switch reviewType {
	case models.ReviewTypeUnknown:
		ctx.ServerError("ReviewType", fmt.Errorf("unknown ReviewType: %s", form.Type))
		return

	// can not approve/reject your own PR
	case models.ReviewTypeApprove, models.ReviewTypeReject:
		if issue.IsPoster(ctx.Doer.ID) {
			var translated string
			if reviewType == models.ReviewTypeApprove {
				translated = ctx.Tr("repo.issues.review.self.approval")
			} else {
				translated = ctx.Tr("repo.issues.review.self.rejection")
			}

			ctx.Flash.Error(translated)
			ctx.Redirect(fmt.Sprintf("%s/pulls/%d/files", ctx.Repo.RepoLink, issue.Index))
			return
		}
	}

	var attachments []string
	if setting.Attachment.Enabled {
		attachments = form.Files
	}

	_, comm, err := pull_service.SubmitReview(ctx, ctx.Doer, ctx.Repo.GitRepo, issue, reviewType, form.Content, form.CommitID, attachments)
	if err != nil {
		if models.IsContentEmptyErr(err) {
			ctx.Flash.Error(ctx.Tr("repo.issues.review.content.empty"))
			ctx.Redirect(fmt.Sprintf("%s/pulls/%d/files", ctx.Repo.RepoLink, issue.Index))
		} else {
			ctx.ServerError("SubmitReview", err)
		}
		return
	}

	ctx.Redirect(fmt.Sprintf("%s/pulls/%d#%s", ctx.Repo.RepoLink, issue.Index, comm.HashTag()))
}

// DismissReview dismissing stale review by repo admin
func DismissReview(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.DismissReviewForm)
	comm, err := pull_service.DismissReview(ctx, form.ReviewID, form.Message, ctx.Doer, true)
	if err != nil {
		ctx.ServerError("pull_service.DismissReview", err)
		return
	}

	ctx.Redirect(fmt.Sprintf("%s/pulls/%d#%s", ctx.Repo.RepoLink, comm.Issue.Index, comm.HashTag()))
}

// viewedFilesUpdate Struct to parse the body of a request to update the reviewed files of a PR
// If you want to implement an API to update the review, simply move this struct into modules.
type viewedFilesUpdate struct {
	Files         map[string]bool `json:"files"`
	HeadCommitSHA string          `json:"headCommitSHA"`
}

func UpdateViewedFiles(ctx *context.Context) {
	// Find corresponding PR
	issue := checkPullInfo(ctx)
	if ctx.Written() {
		return
	}
	pull := issue.PullRequest

	var data *viewedFilesUpdate
	err := json.NewDecoder(ctx.Req.Body).Decode(&data)
	if err != nil {
		log.Warn("Attempted to update a review but could not parse request body: %v", err)
		ctx.Resp.WriteHeader(http.StatusBadRequest)
		return
	}

	// Expect the review to have been now if no head commit was supplied
	if data.HeadCommitSHA == "" {
		data.HeadCommitSHA = pull.HeadCommitID
	}

	updatedFiles := make(map[string]pull_model.ViewedState, len(data.Files))
	for file, viewed := range data.Files {

		// Only unviewed and viewed are possible, has-changed can not be set from the outside
		state := pull_model.Unviewed
		if viewed {
			state = pull_model.Viewed
		}
		updatedFiles[file] = state
	}

	if err := pull_model.UpdateReviewState(ctx, ctx.Doer.ID, pull.ID, data.HeadCommitSHA, updatedFiles); err != nil {
		ctx.ServerError("UpdateReview", err)
	}
}
