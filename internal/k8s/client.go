// Package k8s provides a minimal Kubernetes API client for managing crawler
// CronJobs and Jobs. It uses only net/http — no client-go dependency.
//
// Auth priority:
//  1. In-cluster service account (/var/run/secrets/kubernetes.io/serviceaccount/)
//  2. Env vars: GRABR_K8S_SERVER + GRABR_K8S_TOKEN + GRABR_K8S_NAMESPACE
//
// If neither is available, New returns (nil, nil) and callers must handle the
// nil client (k8s features are disabled).
package k8s

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/deechilly/grabr/internal/store"
)

const (
	inClusterTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	inClusterCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	inClusterNSPath    = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	inClusterAPIServer = "https://kubernetes.default.svc"
)

// Client is a thin Kubernetes API client scoped to one namespace.
type Client struct {
	http      *http.Client
	apiServer string
	token     string
	namespace string
	image     string // container image for crawler pods
}

// New returns a configured Client, or (nil, nil) if no k8s credentials are
// available in the environment. A non-nil error means credentials were found
// but could not be loaded.
func New(namespace, crawlerImage string) (*Client, error) {
	// Try in-cluster first.
	if _, err := os.Stat(inClusterTokenPath); err == nil {
		return newInCluster(namespace, crawlerImage)
	}
	// Try env var override (local dev / port-forward).
	server := os.Getenv("GRABR_K8S_SERVER")
	token := os.Getenv("GRABR_K8S_TOKEN")
	if server != "" && token != "" {
		ns := os.Getenv("GRABR_K8S_NAMESPACE")
		if ns == "" {
			ns = namespace
		}
		return &Client{
			http:      &http.Client{Timeout: 15 * time.Second},
			apiServer: strings.TrimRight(server, "/"),
			token:     token,
			namespace: ns,
			image:     crawlerImage,
		}, nil
	}
	return nil, nil
}

func newInCluster(fallbackNS, crawlerImage string) (*Client, error) {
	token, err := os.ReadFile(inClusterTokenPath)
	if err != nil {
		return nil, fmt.Errorf("k8s: read token: %w", err)
	}
	caData, err := os.ReadFile(inClusterCAPath)
	if err != nil {
		return nil, fmt.Errorf("k8s: read ca: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caData)

	ns := fallbackNS
	if raw, err := os.ReadFile(inClusterNSPath); err == nil && len(raw) > 0 {
		ns = strings.TrimSpace(string(raw))
	}

	return &Client{
		http: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool},
			},
		},
		apiServer: inClusterAPIServer,
		token:     strings.TrimSpace(string(token)),
		namespace: ns,
		image:     crawlerImage,
	}, nil
}

// --- CronJob operations ---

// EnsureCronJob creates or fully replaces the CronJob for the given site.
func (c *Client) EnsureCronJob(site *store.Site) error {
	name := cronJobName(site.Slug)
	url := c.batchURL("cronjobs/" + name)

	existing, err := c.get(url)
	if err != nil {
		return err
	}

	cj := c.buildCronJob(site)

	if existing == nil {
		return c.apply(http.MethodPost, c.batchURL("cronjobs"), cj)
	}

	// Preserve resourceVersion for optimistic concurrency on PUT.
	if meta, ok := existing["metadata"].(map[string]any); ok {
		if rv, ok := meta["resourceVersion"].(string); ok {
			cj.Metadata.ResourceVersion = rv
		}
	}
	return c.apply(http.MethodPut, url, cj)
}

// DeleteCronJob removes the CronJob for the given site slug (ignores 404).
func (c *Client) DeleteCronJob(slug string) error {
	_, err := c.do(http.MethodDelete, c.batchURL("cronjobs/"+cronJobName(slug)), nil)
	return ignoreNotFound(err)
}

// SuspendCronJob sets spec.suspend on the named CronJob.
func (c *Client) SuspendCronJob(slug string, suspend bool) error {
	patch := map[string]any{"spec": map[string]any{"suspend": suspend}}
	body, _ := json.Marshal(patch)
	url := c.batchURL("cronjobs/" + cronJobName(slug))
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("k8s patch cronjob %s: %d %s", slug, resp.StatusCode, b)
	}
	return nil
}

// --- Job operations ---

// CreateCrawlJob creates a one-off Job for the given site (manual "Crawl now").
// Returns the created job name.
func (c *Client) CreateCrawlJob(site *store.Site) (string, error) {
	name := manualJobName(site.Slug)
	job := c.buildJob(site, name, true)
	if err := c.apply(http.MethodPost, c.batchURL("jobs"), job); err != nil {
		return "", err
	}
	return name, nil
}

// DeleteJob deletes a Job by name (ignores 404). Also sets propagationPolicy=Background
// so the pod is garbage-collected.
func (c *Client) DeleteJob(name string) error {
	body := []byte(`{"propagationPolicy":"Background"}`)
	req, err := http.NewRequest(http.MethodDelete, c.batchURL("jobs/"+name), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("k8s delete job %s: %d %s", name, resp.StatusCode, b)
	}
	return nil
}

// DeleteJobsByLabel deletes all Jobs (active or not) matching the site slug label.
// Used by the Cancel handler to kill both scheduled and manual jobs.
func (c *Client) DeleteJobsByLabel(slug string) error {
	label := fmt.Sprintf("grabr.io/site-slug=%s", slug)
	url := c.batchURL("jobs") + "?labelSelector=" + label
	raw, err := c.get(url)
	if err != nil || raw == nil {
		return err
	}
	items, _ := raw["items"].([]any)
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if name == "" {
			continue
		}
		if err := c.DeleteJob(name); err != nil {
			return err
		}
	}
	return nil
}

// HasRunningJob returns true if there is an active Job for the given site slug.
// It checks both manual jobs and CronJob-spawned jobs.
func (c *Client) HasRunningJob(slug string) (bool, error) {
	label := fmt.Sprintf("grabr.io/site-slug=%s", slug)
	url := c.batchURL("jobs") + "?labelSelector=" + label + "&fieldSelector=status.active%3E0"
	raw, err := c.get(url)
	if err != nil {
		return false, err
	}
	if raw == nil {
		return false, nil
	}
	items, _ := raw["items"].([]any)
	return len(items) > 0, nil
}

// --- Internal helpers ---

func (c *Client) batchURL(resource string) string {
	return fmt.Sprintf("%s/apis/batch/v1/namespaces/%s/%s", c.apiServer, c.namespace, resource)
}

func (c *Client) get(url string) (map[string]any, error) {
	resp, err := c.do(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("k8s GET %s: %d %s", url, resp.StatusCode, b)
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}

func (c *Client) apply(method, url string, obj any) error {
	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("k8s %s %s: %d %s", method, url, resp.StatusCode, b)
	}
	return nil
}

func (c *Client) do(method, url string, body []byte) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, nil
	}
	return resp, nil
}

func ignoreNotFound(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "404") {
		return nil
	}
	return err
}

// --- Object builders ---

func (c *Client) buildCronJob(site *store.Site) cronJob {
	one := int32(1)
	three := int32(3)
	suspend := !site.Enabled
	return cronJob{
		APIVersion: "batch/v1",
		Kind:       "CronJob",
		Metadata: objectMeta{
			Name:      cronJobName(site.Slug),
			Namespace: c.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "grabr",
				"grabr.io/site-slug":           site.Slug,
			},
		},
		Spec: cronJobSpec{
			Schedule:                   intervalToCron(site.IntervalSeconds),
			ConcurrencyPolicy:          "Forbid",
			Suspend:                    &suspend,
			SuccessfulJobsHistoryLimit: &one,
			FailedJobsHistoryLimit:     &three,
			JobTemplate: jobTemplateSpec{
				Metadata: objectMeta{Labels: map[string]string{"grabr.io/site-slug": site.Slug}},
				Spec:     c.buildJobSpec(site),
			},
		},
	}
}

func (c *Client) buildJob(site *store.Site, name string, manual bool) job {
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "grabr",
		"grabr.io/site-slug":           site.Slug,
	}
	if manual {
		labels["grabr.io/manual"] = "true"
	}
	return job{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata:   objectMeta{Name: name, Namespace: c.namespace, Labels: labels},
		Spec:       c.buildJobSpec(site),
	}
}

func (c *Client) buildJobSpec(site *store.Site) jobSpec {
	zero := int32(0)
	ttl := int32(3600)
	return jobSpec{
		BackoffLimit:            &zero,
		TTLSecondsAfterFinished: &ttl,
		Template: podTemplateSpec{
			Metadata: objectMeta{
				Labels: map[string]string{"grabr.io/site-slug": site.Slug},
			},
			Spec: podSpec{
				RestartPolicy: "Never",
				Containers: []container{{
					Name:            "grabr-crawl",
					Image:           c.image,
					ImagePullPolicy: "Never",
					Args:            []string{"crawl", "--site-id", strconv.FormatInt(site.ID, 10)},
					Env: []envVar{
						{
							Name: "GRABR_DATABASE_URL",
							ValueFrom: &envVarSource{SecretKeyRef: &secretKeySelector{
								Name: "grabr-db",
								Key:  "DATABASE_URL",
							}},
						},
						{Name: "GRABR_DATA_DIR", Value: "/data"},
					},
					VolumeMounts: []volumeMount{{Name: "mirrors", MountPath: "/data"}},
				}},
				Volumes: []volume{{
					Name: "mirrors",
					PersistentVolumeClaim: &pvcVolumeSource{ClaimName: "grabr-mirrors"},
				}},
			},
		},
	}
}

// --- Naming ---

func cronJobName(slug string) string {
	if len(slug) > 40 {
		slug = slug[:40]
	}
	return "grabr-crawl-" + slug
}

func manualJobName(slug string) string {
	return fmt.Sprintf("grabr-manual-%s-%d", truncate(slug, 30), time.Now().Unix())
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// intervalToCron converts a seconds interval to a cron expression.
//
// Note: cron `*/N` resets at each unit boundary, so non-divisor intervals
// don't strictly fire every N units. E.g. `*/17 * * * *` fires at :00, :17,
// :34, :51 and then jumps to :00 of the next hour (a 9-minute gap). For
// grabr's typical daily-ish intervals this is acceptable; callers who need
// strict periodicity should snap their interval to a divisor of the next-larger
// unit (60, 30, 20, 15, 12, 10, ... minutes; 24, 12, 8, 6, ... hours).
func intervalToCron(seconds int) string {
	if seconds < 60 {
		seconds = 60
	}
	mins := seconds / 60
	if mins < 60 {
		return fmt.Sprintf("*/%d * * * *", mins)
	}
	hours := mins / 60
	if hours < 24 {
		return fmt.Sprintf("0 */%d * * *", hours)
	}
	days := hours / 24
	return fmt.Sprintf("0 0 */%d * *", days)
}

// --- JSON types (minimal subset of the Kubernetes API) ---

type cronJob struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   objectMeta  `json:"metadata"`
	Spec       cronJobSpec `json:"spec"`
}

type cronJobSpec struct {
	Schedule                   string          `json:"schedule"`
	ConcurrencyPolicy          string          `json:"concurrencyPolicy"`
	Suspend                    *bool           `json:"suspend,omitempty"`
	SuccessfulJobsHistoryLimit *int32          `json:"successfulJobsHistoryLimit,omitempty"`
	FailedJobsHistoryLimit     *int32          `json:"failedJobsHistoryLimit,omitempty"`
	JobTemplate                jobTemplateSpec `json:"jobTemplate"`
}

type jobTemplateSpec struct {
	Metadata objectMeta `json:"metadata,omitempty"`
	Spec     jobSpec    `json:"spec"`
}

type job struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   objectMeta `json:"metadata"`
	Spec       jobSpec    `json:"spec"`
}

type jobSpec struct {
	BackoffLimit            *int32          `json:"backoffLimit,omitempty"`
	TTLSecondsAfterFinished *int32          `json:"ttlSecondsAfterFinished,omitempty"`
	Template                podTemplateSpec `json:"template"`
}

type podTemplateSpec struct {
	Metadata objectMeta `json:"metadata,omitempty"`
	Spec     podSpec    `json:"spec"`
}

type podSpec struct {
	RestartPolicy string      `json:"restartPolicy"`
	Containers    []container `json:"containers"`
	Volumes       []volume    `json:"volumes,omitempty"`
}

type container struct {
	Name            string        `json:"name"`
	Image           string        `json:"image"`
	ImagePullPolicy string        `json:"imagePullPolicy,omitempty"`
	Args            []string      `json:"args,omitempty"`
	Env             []envVar      `json:"env,omitempty"`
	VolumeMounts    []volumeMount `json:"volumeMounts,omitempty"`
}

type envVar struct {
	Name      string        `json:"name"`
	Value     string        `json:"value,omitempty"`
	ValueFrom *envVarSource `json:"valueFrom,omitempty"`
}

type envVarSource struct {
	SecretKeyRef *secretKeySelector `json:"secretKeyRef,omitempty"`
}

type secretKeySelector struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type volume struct {
	Name                  string           `json:"name"`
	PersistentVolumeClaim *pvcVolumeSource `json:"persistentVolumeClaim,omitempty"`
}

type pvcVolumeSource struct {
	ClaimName string `json:"claimName"`
}

type volumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

type objectMeta struct {
	Name            string            `json:"name,omitempty"`
	Namespace       string            `json:"namespace,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
}
