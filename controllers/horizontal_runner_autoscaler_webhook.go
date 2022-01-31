/*
Copyright 2020 The actions-runner-controller authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-logr/logr"
	gogithub "github.com/google/go-github/v39/github"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/actions-runner-controller/actions-runner-controller/api/v1alpha1"
	"github.com/actions-runner-controller/actions-runner-controller/github"
)

const (
	scaleTargetKey = "scaleTarget"

	keyPrefixEnterprise = "enterprises/"
	keyRunnerGroup      = "/group/"
)

// HorizontalRunnerAutoscalerGitHubWebhook autoscales a HorizontalRunnerAutoscaler and the RunnerDeployment on each
// GitHub Webhook received
type HorizontalRunnerAutoscalerGitHubWebhook struct {
	client.Client
	Log      logr.Logger
	Recorder record.EventRecorder
	Scheme   *runtime.Scheme

	// SecretKeyBytes is the byte representation of the Webhook secret token
	// the administrator is generated and specified in GitHub Web UI.
	SecretKeyBytes []byte

	// GitHub Client to discover runner groups assigned to a repository
	GitHubClient *github.Client

	// Namespace is the namespace to watch for HorizontalRunnerAutoscaler's to be
	// scaled on Webhook.
	// Set to empty for letting it watch for all namespaces.
	Namespace string
	Name      string
}

func (autoscaler *HorizontalRunnerAutoscalerGitHubWebhook) Reconcile(_ context.Context, request reconcile.Request) (reconcile.Result, error) {
	return ctrl.Result{}, nil
}

// +kubebuilder:rbac:groups=actions.summerwind.dev,resources=horizontalrunnerautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=actions.summerwind.dev,resources=horizontalrunnerautoscalers/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=actions.summerwind.dev,resources=horizontalrunnerautoscalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (autoscaler *HorizontalRunnerAutoscalerGitHubWebhook) Handle(w http.ResponseWriter, r *http.Request) {
	var (
		ok bool

		err error
	)

	defer func() {
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)

			if err != nil {
				msg := err.Error()
				if written, err := w.Write([]byte(msg)); err != nil {
					autoscaler.Log.Error(err, "failed writing http error response", "msg", msg, "written", written)
				}
			}
		}
	}()

	defer func() {
		if r.Body != nil {
			r.Body.Close()
		}
	}()

	// respond ok to GET / e.g. for health check
	if r.Method == http.MethodGet {
		ok = true
		fmt.Fprintln(w, "webhook server is running")
		return
	}

	var payload []byte

	if len(autoscaler.SecretKeyBytes) > 0 {
		payload, err = gogithub.ValidatePayload(r, autoscaler.SecretKeyBytes)
		if err != nil {
			autoscaler.Log.Error(err, "error validating request body")

			return
		}
	} else {
		payload, err = ioutil.ReadAll(r.Body)
		if err != nil {
			autoscaler.Log.Error(err, "error reading request body")

			return
		}
	}

	webhookType := gogithub.WebHookType(r)
	event, err := gogithub.ParseWebHook(webhookType, payload)
	if err != nil {
		var s string
		if payload != nil {
			s = string(payload)
		}

		autoscaler.Log.Error(err, "could not parse webhook", "webhookType", webhookType, "payload", s)

		return
	}

	var target *ScaleTarget

	log := autoscaler.Log.WithValues(
		"event", webhookType,
		"hookID", r.Header.Get("X-GitHub-Hook-ID"),
		"delivery", r.Header.Get("X-GitHub-Delivery"),
	)

	var enterpriseEvent struct {
		Enterprise struct {
			Slug string `json:"slug,omitempty"`
		} `json:"enterprise,omitempty"`
	}
	if err := json.Unmarshal(payload, &enterpriseEvent); err != nil {
		var s string
		if payload != nil {
			s = string(payload)
		}
		autoscaler.Log.Error(err, "could not parse webhook payload for extracting enterprise slug", "webhookType", webhookType, "payload", s)
	}
	enterpriseSlug := enterpriseEvent.Enterprise.Slug

	switch e := event.(type) {
	case *gogithub.PushEvent:
		target, err = autoscaler.getScaleUpTarget(
			context.TODO(),
			log,
			e.Repo.GetName(),
			e.Repo.Owner.GetLogin(),
			e.Repo.Owner.GetType(),
			// Most go-github Event types don't seem to contain Enteprirse(.Slug) fields
			// we need, so we parse it by ourselves.
			enterpriseSlug,
			autoscaler.MatchPushEvent(e),
		)
	case *gogithub.PullRequestEvent:
		target, err = autoscaler.getScaleUpTarget(
			context.TODO(),
			log,
			e.Repo.GetName(),
			e.Repo.Owner.GetLogin(),
			e.Repo.Owner.GetType(),
			// Most go-github Event types don't seem to contain Enteprirse(.Slug) fields
			// we need, so we parse it by ourselves.
			enterpriseSlug,
			autoscaler.MatchPullRequestEvent(e),
		)

		if pullRequest := e.PullRequest; pullRequest != nil {
			log = log.WithValues(
				"pullRequest.base.ref", e.PullRequest.Base.GetRef(),
				"action", e.GetAction(),
			)
		}
	case *gogithub.CheckRunEvent:
		target, err = autoscaler.getScaleUpTarget(
			context.TODO(),
			log,
			e.Repo.GetName(),
			e.Repo.Owner.GetLogin(),
			e.Repo.Owner.GetType(),
			// Most go-github Event types don't seem to contain Enteprirse(.Slug) fields
			// we need, so we parse it by ourselves.
			enterpriseSlug,
			autoscaler.MatchCheckRunEvent(e),
		)

		if checkRun := e.GetCheckRun(); checkRun != nil {
			log = log.WithValues(
				"checkRun.status", checkRun.GetStatus(),
				"action", e.GetAction(),
			)
		}
	case *gogithub.WorkflowJobEvent:
		if workflowJob := e.GetWorkflowJob(); workflowJob != nil {
			log = log.WithValues(
				"workflowJob.status", workflowJob.GetStatus(),
				"workflowJob.labels", workflowJob.Labels,
				"repository.name", e.Repo.GetName(),
				"repository.owner.login", e.Repo.Owner.GetLogin(),
				"repository.owner.type", e.Repo.Owner.GetType(),
				"enterprise.slug", enterpriseSlug,
				"action", e.GetAction(),
			)
		}

		labels := e.WorkflowJob.Labels

		switch action := e.GetAction(); action {
		case "queued", "completed":
			target, err = autoscaler.getJobScaleUpTargetForRepoOrOrg(
				context.TODO(),
				log,
				e.Repo.GetName(),
				e.Repo.Owner.GetLogin(),
				e.Repo.Owner.GetType(),
				enterpriseSlug,
				labels,
			)

			if target != nil {
				if e.GetAction() == "queued" {
					target.Amount = 1
				} else if e.GetAction() == "completed" {
					// A nagative amount is processed in the tryScale func as a scale-down request,
					// that erasese the oldest CapacityReservation with the same amount.
					// If the first CapacityReservation was with Replicas=1, this negative scale target erases that,
					// so that the resulting desired replicas decreases by 1.
					target.Amount = -1
				}
			}
		default:
			ok = true

			w.WriteHeader(http.StatusOK)

			log.V(2).Info("Received and ignored a workflow_job event as it triggers neither scale-up nor scale-down", "action", action)

			return
		}
	case *gogithub.PingEvent:
		ok = true

		w.WriteHeader(http.StatusOK)

		msg := "pong"

		if written, err := w.Write([]byte(msg)); err != nil {
			log.Error(err, "failed writing http response", "msg", msg, "written", written)
		}

		log.Info("received ping event")

		return
	default:
		log.Info("unknown event type", "eventType", webhookType)

		return
	}

	if err != nil {
		log.Error(err, "handling check_run event")

		return
	}

	if target == nil {
		log.Info(
			"Scale target not found. If this is unexpected, ensure that there is exactly one repository-wide or organizational runner deployment that matches this webhook event",
		)

		msg := "no horizontalrunnerautoscaler to scale for this github event"

		ok = true

		w.WriteHeader(http.StatusOK)

		if written, err := w.Write([]byte(msg)); err != nil {
			log.Error(err, "failed writing http response", "msg", msg, "written", written)
		}

		return
	}

	if err := autoscaler.tryScale(context.TODO(), target); err != nil {
		log.Error(err, "could not scale up")

		return
	}

	ok = true

	w.WriteHeader(http.StatusOK)

	msg := fmt.Sprintf("scaled %s by %d", target.Name, target.Amount)

	autoscaler.Log.Info(msg)

	if written, err := w.Write([]byte(msg)); err != nil {
		log.Error(err, "failed writing http response", "msg", msg, "written", written)
	}
}

func (autoscaler *HorizontalRunnerAutoscalerGitHubWebhook) findHRAsByKey(ctx context.Context, value string) ([]v1alpha1.HorizontalRunnerAutoscaler, error) {
	ns := autoscaler.Namespace

	var defaultListOpts []client.ListOption

	if ns != "" {
		defaultListOpts = append(defaultListOpts, client.InNamespace(ns))
	}

	var hras []v1alpha1.HorizontalRunnerAutoscaler

	if value != "" {
		opts := append([]client.ListOption{}, defaultListOpts...)
		opts = append(opts, client.MatchingFields{scaleTargetKey: value})

		if autoscaler.Namespace != "" {
			opts = append(opts, client.InNamespace(autoscaler.Namespace))
		}

		var hraList v1alpha1.HorizontalRunnerAutoscalerList

		if err := autoscaler.List(ctx, &hraList, opts...); err != nil {
			return nil, err
		}

		for _, d := range hraList.Items {
			hras = append(hras, d)
		}
	}

	return hras, nil
}

func matchTriggerConditionAgainstEvent(types []string, eventAction *string) bool {
	if len(types) == 0 {
		return true
	}

	if eventAction == nil {
		return false
	}

	for _, tpe := range types {
		if tpe == *eventAction {
			return true
		}
	}

	return false
}

type ScaleTarget struct {
	v1alpha1.HorizontalRunnerAutoscaler
	v1alpha1.ScaleUpTrigger
}

func (autoscaler *HorizontalRunnerAutoscalerGitHubWebhook) searchScaleTargets(hras []v1alpha1.HorizontalRunnerAutoscaler, f func(v1alpha1.ScaleUpTrigger) bool) []ScaleTarget {
	var matched []ScaleTarget

	for _, hra := range hras {
		if !hra.ObjectMeta.DeletionTimestamp.IsZero() {
			continue
		}

		for _, scaleUpTrigger := range hra.Spec.ScaleUpTriggers {
			if !f(scaleUpTrigger) {
				continue
			}

			matched = append(matched, ScaleTarget{
				HorizontalRunnerAutoscaler: hra,
				ScaleUpTrigger:             scaleUpTrigger,
			})
		}
	}

	return matched
}

func (autoscaler *HorizontalRunnerAutoscalerGitHubWebhook) getScaleTarget(ctx context.Context, name string, f func(v1alpha1.ScaleUpTrigger) bool) (*ScaleTarget, error) {
	hras, err := autoscaler.findHRAsByKey(ctx, name)
	if err != nil {
		return nil, err
	}

	autoscaler.Log.V(1).Info(fmt.Sprintf("Found %d HRAs by key", len(hras)), "key", name)

	targets := autoscaler.searchScaleTargets(hras, f)

	n := len(targets)

	if n == 0 {
		return nil, nil
	}

	if n > 1 {
		var scaleTargetIDs []string

		for _, t := range targets {
			scaleTargetIDs = append(scaleTargetIDs, t.HorizontalRunnerAutoscaler.Name)
		}

		autoscaler.Log.Info(
			"Found too many scale targets: "+
				"It must be exactly one to avoid ambiguity. "+
				"Either set Namespace for the webhook-based autoscaler to let it only find HRAs in the namespace, "+
				"or update Repository, Organization, or Enterprise fields in your RunnerDeployment resources to fix the ambiguity.",
			"scaleTargets", strings.Join(scaleTargetIDs, ","))

		return nil, nil
	}

	return &targets[0], nil
}

func (autoscaler *HorizontalRunnerAutoscalerGitHubWebhook) getScaleUpTarget(ctx context.Context, log logr.Logger, repo, owner, ownerType, enterprise string, f func(v1alpha1.ScaleUpTrigger) bool) (*ScaleTarget, error) {
	scaleTarget := func(value string) (*ScaleTarget, error) {
		return autoscaler.getScaleTarget(ctx, value, f)
	}
	return autoscaler.getScaleUpTargetWithFunction(ctx, log, repo, owner, ownerType, enterprise, scaleTarget)
}

func (autoscaler *HorizontalRunnerAutoscalerGitHubWebhook) getJobScaleUpTargetForRepoOrOrg(
	ctx context.Context, log logr.Logger, repo, owner, ownerType, enterprise string, labels []string,
) (*ScaleTarget, error) {

	scaleTarget := func(value string) (*ScaleTarget, error) {
		return autoscaler.getJobScaleTarget(ctx, value, labels)
	}
	return autoscaler.getScaleUpTargetWithFunction(ctx, log, repo, owner, ownerType, enterprise, scaleTarget)
}

func (autoscaler *HorizontalRunnerAutoscalerGitHubWebhook) getScaleUpTargetWithFunction(
	ctx context.Context, log logr.Logger, repo, owner, ownerType, enterprise string, scaleTarget func(value string) (*ScaleTarget, error)) (*ScaleTarget, error) {

	repositoryRunnerKey := owner + "/" + repo

	// Search for repository HRAs
	if target, err := scaleTarget(repositoryRunnerKey); err != nil {
		log.Error(err, "finding repository-wide runner", "repository", repositoryRunnerKey)
		return nil, err
	} else if target != nil {
		log.Info("job scale up target is repository-wide runners", "repository", repo)
		return target, nil
	}

	if ownerType == "User" {
		log.V(1).Info("user repositories not supported", "owner", owner)
		return nil, nil
	}

	// Search for organization runner HRAs in default runner group
	if target, err := scaleTarget(owner); err != nil {
		log.Error(err, "finding organizational runner", "organization", owner)
		return nil, err
	} else if target != nil {
		log.Info("job scale up target is organizational runners", "organization", owner)
		return target, nil
	}

	if enterprise != "" {
		// Search for enterprise runner HRAs in default runner group
		if target, err := scaleTarget(enterpriseKey(enterprise)); err != nil {
			log.Error(err, "finding enterprise runner", "enterprise", enterprise)
			return nil, err
		} else if target != nil {
			log.Info("scale up target is default enterprise runners", "enterprise", enterprise)
			return target, nil
		}
	}

	// At this point there were no default organization/enterprise runners available to use, try now
	// searching in runner groups

	// We need to get the potential runner groups first to avoid spending API queries needless. Once/if GitHub improves an
	// API to find related/linked runner groups from a specific repository this logic could be removed
	availableEnterpriseGroups, availableOrganizationGroups, err := autoscaler.getPotentialGroupsFromHRAs(ctx, enterprise, owner)
	if err != nil {
		log.Error(err, "finding potential organization runner groups from HRAs", "organization", owner)
		return nil, err
	}
	if len(availableEnterpriseGroups) == 0 && len(availableOrganizationGroups) == 0 {
		log.V(1).Info("no repository/organizational/enterprise runner found",
			"repository", repositoryRunnerKey,
			"organization", owner,
			"enterprises", enterprise,
		)
	}

	var enterpriseGroups []string
	var organizationGroups []string
	if autoscaler.GitHubClient != nil {
		// Get available organization runner groups and enterprise runner groups for a repository
		// These are the sum of runner groups with repository access = All repositories plus
		// runner groups where owner/repo has access to
		enterpriseGroups, organizationGroups, err = autoscaler.GitHubClient.GetRunnerGroupsFromRepository(ctx, owner, repositoryRunnerKey, availableEnterpriseGroups, availableOrganizationGroups)
		log.V(1).Info("Searching in runner groups", "enterprise.groups", enterpriseGroups, "organization.groups", organizationGroups)
		if err != nil {
			log.Error(err, "Unable to find runner groups from repository", "organization", owner, "repository", repo)
			return nil, nil
		}
	} else {
		// For backwards compatibility if GitHub authentication is not configured, we assume all runner groups have
		// visibility=all to honor the previous implementation, therefore any available enterprise/organization runner
		// is a potential target for scaling
		enterpriseGroups = availableEnterpriseGroups
		organizationGroups = availableOrganizationGroups
	}

	for _, group := range organizationGroups {
		if target, err := scaleTarget(organizationalRunnerGroupKey(owner, group)); err != nil {
			log.Error(err, "finding organizational runner group", "organization", owner)
			return nil, err
		} else if target != nil {
			log.Info(fmt.Sprintf("job scale up target is organizational runner group %s", target.Name), "organization", owner)
			return target, nil
		}
	}

	for _, group := range enterpriseGroups {
		if target, err := scaleTarget(enterpriseRunnerGroupKey(enterprise, group)); err != nil {
			log.Error(err, "finding enterprise runner group", "enterprise", owner)
			return nil, err
		} else if target != nil {
			log.Info(fmt.Sprintf("job scale up target is enterprise runner group %s", target.Name), "enterprise", owner)
			return target, nil
		}
	}

	log.V(1).Info("no repository/organizational/enterprise runner found",
		"repository", repositoryRunnerKey,
		"organization", owner,
		"enterprises", enterprise,
	)
	return nil, nil
}

func (autoscaler *HorizontalRunnerAutoscalerGitHubWebhook) getPotentialGroupsFromHRAs(ctx context.Context, enterprise, org string) ([]string, []string, error) {
	var enterpriseRunnerGroups []string
	var orgRunnerGroups []string
	ns := autoscaler.Namespace

	var defaultListOpts []client.ListOption
	if ns != "" {
		defaultListOpts = append(defaultListOpts, client.InNamespace(ns))
	}

	opts := append([]client.ListOption{}, defaultListOpts...)
	if autoscaler.Namespace != "" {
		opts = append(opts, client.InNamespace(autoscaler.Namespace))
	}

	var hraList v1alpha1.HorizontalRunnerAutoscalerList
	if err := autoscaler.List(ctx, &hraList, opts...); err != nil {
		return orgRunnerGroups, enterpriseRunnerGroups, err
	}

	for _, hra := range hraList.Items {
		switch hra.Spec.ScaleTargetRef.Kind {
		case "RunnerSet":
			var rs v1alpha1.RunnerSet
			if err := autoscaler.Client.Get(context.Background(), types.NamespacedName{Namespace: hra.Namespace, Name: hra.Spec.ScaleTargetRef.Name}, &rs); err != nil {
				return orgRunnerGroups, enterpriseRunnerGroups, err
			}
			if rs.Spec.Organization == org && rs.Spec.Group != "" {
				orgRunnerGroups = append(orgRunnerGroups, rs.Spec.Group)
			}
			if rs.Spec.Enterprise == enterprise && rs.Spec.Group != "" {
				enterpriseRunnerGroups = append(enterpriseRunnerGroups, rs.Spec.Group)
			}
		case "RunnerDeployment", "":
			var rd v1alpha1.RunnerDeployment
			if err := autoscaler.Client.Get(context.Background(), types.NamespacedName{Namespace: hra.Namespace, Name: hra.Spec.ScaleTargetRef.Name}, &rd); err != nil {
				return orgRunnerGroups, enterpriseRunnerGroups, err
			}
			if rd.Spec.Template.Spec.Organization == org && rd.Spec.Template.Spec.Group != "" {
				orgRunnerGroups = append(orgRunnerGroups, rd.Spec.Template.Spec.Group)
			}
			if rd.Spec.Template.Spec.Enterprise == enterprise && rd.Spec.Template.Spec.Group != "" {
				enterpriseRunnerGroups = append(enterpriseRunnerGroups, rd.Spec.Template.Spec.Group)
			}
		}
	}
	return enterpriseRunnerGroups, orgRunnerGroups, nil
}

func (autoscaler *HorizontalRunnerAutoscalerGitHubWebhook) getJobScaleTarget(ctx context.Context, name string, labels []string) (*ScaleTarget, error) {
	hras, err := autoscaler.findHRAsByKey(ctx, name)
	if err != nil {
		return nil, err
	}

	autoscaler.Log.V(1).Info(fmt.Sprintf("Found %d HRAs by key", len(hras)), "key", name)

HRA:
	for _, hra := range hras {
		if !hra.ObjectMeta.DeletionTimestamp.IsZero() {
			continue
		}

		if len(hra.Spec.ScaleUpTriggers) > 1 {
			autoscaler.Log.V(1).Info("Skipping this HRA as it has too many ScaleUpTriggers to be used in workflow_job based scaling", "hra", hra.Name)

			continue
		}

		var duration metav1.Duration

		if len(hra.Spec.ScaleUpTriggers) > 0 {
			duration = hra.Spec.ScaleUpTriggers[0].Duration
		}

		if duration.Duration <= 0 {
			// Try to release the reserved capacity after at least 10 minutes by default,
			// we won't end up in the reserved capacity remained forever in case GitHub somehow stopped sending us "completed" workflow_job events.
			// GitHub usually send us those but nothing is 100% guaranteed, e.g. in case of something went wrong on GitHub :)
			// Probably we'd better make this configurable via custom resources in the future?
			duration.Duration = 10 * time.Minute
		}

		switch hra.Spec.ScaleTargetRef.Kind {
		case "RunnerSet":
			var rs v1alpha1.RunnerSet

			if err := autoscaler.Client.Get(context.Background(), types.NamespacedName{Namespace: hra.Namespace, Name: hra.Spec.ScaleTargetRef.Name}, &rs); err != nil {
				return nil, err
			}

			// Ensure that the RunnerSet-managed runners have all the labels requested by the workflow_job.
			for _, l := range labels {
				var matched bool

				// ignore "self-hosted" label as all instance here are self-hosted
				if l == "self-hosted" {
					continue
				}

				// TODO labels related to OS and architecture needs to be explicitly declared or the current implementation will not be able to find them.

				for _, l2 := range rs.Spec.Labels {
					if l == l2 {
						matched = true
						break
					}
				}

				if !matched {
					continue HRA
				}
			}

			return &ScaleTarget{HorizontalRunnerAutoscaler: hra, ScaleUpTrigger: v1alpha1.ScaleUpTrigger{Duration: duration}}, nil
		case "RunnerDeployment", "":
			var rd v1alpha1.RunnerDeployment

			if err := autoscaler.Client.Get(context.Background(), types.NamespacedName{Namespace: hra.Namespace, Name: hra.Spec.ScaleTargetRef.Name}, &rd); err != nil {
				return nil, err
			}

			// Ensure that the RunnerDeployment-managed runners have all the labels requested by the workflow_job.
			for _, l := range labels {
				var matched bool

				// ignore "self-hosted" label as all instance here are self-hosted
				if l == "self-hosted" {
					continue
				}

				// TODO labels related to OS and architecture needs to be explicitly declared or the current implementation will not be able to find them.

				for _, l2 := range rd.Spec.Template.Spec.Labels {
					if l == l2 {
						matched = true
						break
					}
				}

				if !matched {
					continue HRA
				}
			}

			return &ScaleTarget{HorizontalRunnerAutoscaler: hra, ScaleUpTrigger: v1alpha1.ScaleUpTrigger{Duration: duration}}, nil
		default:
			return nil, fmt.Errorf("unsupported scaleTargetRef.kind: %v", hra.Spec.ScaleTargetRef.Kind)
		}
	}

	return nil, nil
}

func (autoscaler *HorizontalRunnerAutoscalerGitHubWebhook) tryScale(ctx context.Context, target *ScaleTarget) error {
	if target == nil {
		return nil
	}

	copy := target.HorizontalRunnerAutoscaler.DeepCopy()

	amount := 1

	if target.ScaleUpTrigger.Amount != 0 {
		amount = target.ScaleUpTrigger.Amount
	}

	capacityReservations := getValidCapacityReservations(copy)

	if amount > 0 {
		copy.Spec.CapacityReservations = append(capacityReservations, v1alpha1.CapacityReservation{
			ExpirationTime: metav1.Time{Time: time.Now().Add(target.ScaleUpTrigger.Duration.Duration)},
			Replicas:       amount,
		})
	} else if amount < 0 {
		var reservations []v1alpha1.CapacityReservation

		var found bool

		for _, r := range capacityReservations {
			if !found && r.Replicas+amount == 0 {
				found = true
			} else {
				reservations = append(reservations, r)
			}
		}

		copy.Spec.CapacityReservations = reservations
	}

	autoscaler.Log.Info(
		"Patching hra for capacityReservations update",
		"before", target.HorizontalRunnerAutoscaler.Spec.CapacityReservations,
		"after", copy.Spec.CapacityReservations,
	)

	if err := autoscaler.Client.Patch(ctx, copy, client.MergeFrom(&target.HorizontalRunnerAutoscaler)); err != nil {
		return fmt.Errorf("patching horizontalrunnerautoscaler to add capacity reservation: %w", err)
	}

	return nil
}

func getValidCapacityReservations(autoscaler *v1alpha1.HorizontalRunnerAutoscaler) []v1alpha1.CapacityReservation {
	var capacityReservations []v1alpha1.CapacityReservation

	now := time.Now()

	for _, reservation := range autoscaler.Spec.CapacityReservations {
		if reservation.ExpirationTime.Time.After(now) {
			capacityReservations = append(capacityReservations, reservation)
		}
	}

	return capacityReservations
}

func (autoscaler *HorizontalRunnerAutoscalerGitHubWebhook) SetupWithManager(mgr ctrl.Manager) error {
	name := "webhookbasedautoscaler"
	if autoscaler.Name != "" {
		name = autoscaler.Name
	}

	autoscaler.Recorder = mgr.GetEventRecorderFor(name)

	if err := mgr.GetFieldIndexer().IndexField(context.TODO(), &v1alpha1.HorizontalRunnerAutoscaler{}, scaleTargetKey, func(rawObj client.Object) []string {
		hra := rawObj.(*v1alpha1.HorizontalRunnerAutoscaler)

		if hra.Spec.ScaleTargetRef.Name == "" {
			return nil
		}

		switch hra.Spec.ScaleTargetRef.Kind {
		case "", "RunnerDeployment":
			var rd v1alpha1.RunnerDeployment
			if err := autoscaler.Client.Get(context.Background(), types.NamespacedName{Namespace: hra.Namespace, Name: hra.Spec.ScaleTargetRef.Name}, &rd); err != nil {
				autoscaler.Log.V(1).Info(fmt.Sprintf("RunnerDeployment not found with scale target ref name %s for hra %s", hra.Spec.ScaleTargetRef.Name, hra.Name))
				return nil
			}

			keys := []string{}
			if rd.Spec.Template.Spec.Repository != "" {
				keys = append(keys, rd.Spec.Template.Spec.Repository) // Repository runners
			}
			if rd.Spec.Template.Spec.Organization != "" {
				if group := rd.Spec.Template.Spec.Group; group != "" {
					keys = append(keys, organizationalRunnerGroupKey(rd.Spec.Template.Spec.Organization, rd.Spec.Template.Spec.Group)) // Organization runner groups
				} else {
					keys = append(keys, rd.Spec.Template.Spec.Organization) // Organization runners
				}
			}
			if enterprise := rd.Spec.Template.Spec.Enterprise; enterprise != "" {
				if group := rd.Spec.Template.Spec.Group; group != "" {
					keys = append(keys, enterpriseRunnerGroupKey(enterprise, rd.Spec.Template.Spec.Group)) // Enterprise runner groups
				} else {
					keys = append(keys, enterpriseKey(enterprise)) // Enterprise runners
				}
			}
			autoscaler.Log.V(1).Info(fmt.Sprintf("HRA keys indexed for HRA %s: %v", hra.Name, keys))
			return keys
		case "RunnerSet":
			var rs v1alpha1.RunnerSet
			if err := autoscaler.Client.Get(context.Background(), types.NamespacedName{Namespace: hra.Namespace, Name: hra.Spec.ScaleTargetRef.Name}, &rs); err != nil {
				autoscaler.Log.V(1).Info(fmt.Sprintf("RunnerSet not found with scale target ref name %s for hra %s", hra.Spec.ScaleTargetRef.Name, hra.Name))
				return nil
			}

			keys := []string{}
			if rs.Spec.Repository != "" {
				keys = append(keys, rs.Spec.Repository) // Repository runners
			}
			if rs.Spec.Organization != "" {
				keys = append(keys, rs.Spec.Organization) // Organization runners
				if group := rs.Spec.Group; group != "" {
					keys = append(keys, organizationalRunnerGroupKey(rs.Spec.Organization, rs.Spec.Group)) // Organization runner groups
				}
			}
			if enterprise := rs.Spec.Enterprise; enterprise != "" {
				keys = append(keys, enterpriseKey(enterprise)) // Enterprise runners
				if group := rs.Spec.Group; group != "" {
					keys = append(keys, enterpriseRunnerGroupKey(enterprise, rs.Spec.Group)) // Enterprise runner groups
				}
			}
			autoscaler.Log.V(1).Info(fmt.Sprintf("HRA keys indexed for HRA %s: %v", hra.Name, keys))
			return keys
		}

		return nil
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.HorizontalRunnerAutoscaler{}).
		Named(name).
		Complete(autoscaler)
}

func enterpriseKey(name string) string {
	return keyPrefixEnterprise + name
}

func organizationalRunnerGroupKey(owner, group string) string {
	return owner + keyRunnerGroup + group
}

func enterpriseRunnerGroupKey(enterprise, group string) string {
	return keyPrefixEnterprise + enterprise + keyRunnerGroup + group
}
