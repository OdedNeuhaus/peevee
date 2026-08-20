package collector

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/OdedNeuhaus/peevee/internal/cluster"
	"github.com/OdedNeuhaus/peevee/internal/model"
)

func boundPVC(ns, name string) *corev1.PersistentVolumeClaim {
	fs := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: "pv-" + name,
			VolumeMode: &fs,
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

// A claim on a node peevee could not scrape is not an orphan. Reporting it as
// "unmounted" is the bug behind issue #4: it sends whoever reads it looking for
// a claim nobody mounts, when the real cause is on the node.
func TestUnscrapableNodeIsErrorNotUnmounted(t *testing.T) {
	c := &Collector{}
	cl := &cluster.Cluster{Name: "prod"}
	mounts := map[string]mountInfo{
		"team/data": {pods: []string{"api-0"}, node: "node-1"},
	}
	scrape := scrapeResult{
		samples: map[string]volumeSample{},
		failed:  map[string]string{"node-1": "not authorised to read nodes/proxy on this cluster"},
	}

	v := c.buildVolume(cl, boundPVC("team", "data"), nil, mounts, scrape, 0)

	if v.Status != model.StatusError {
		t.Fatalf("status = %q, want %q", v.Status, model.StatusError)
	}
	if !strings.Contains(v.Message, "node-1") {
		t.Errorf("message does not name the node: %q", v.Message)
	}
	if !strings.Contains(v.Message, "nodes/proxy") {
		t.Errorf("message does not carry the reason: %q", v.Message)
	}
	if v.HasStats {
		t.Error("a claim with no sample must not claim to have stats")
	}
}

// A claim with a pod on it, whose node answered and had nothing to say about
// this volume, is in use. Reporting it as "unmounted" is issue #4: it describes
// the claim when the fact belongs to the driver.
func TestMountedButUnreportedIsNotUnmounted(t *testing.T) {
	c := &Collector{}
	cl := &cluster.Cluster{Name: "prod"}
	mounts := map[string]mountInfo{
		"team/data": {pods: []string{"api-0"}, node: "node-1"},
	}
	scrape := scrapeResult{samples: map[string]volumeSample{}, failed: map[string]string{}}

	v := c.buildVolume(cl, boundPVC("team", "data"), nil, mounts, scrape, 0)

	if v.Status != model.StatusUnreported {
		t.Fatalf("status = %q, want %q", v.Status, model.StatusUnreported)
	}
	if v.HasStats {
		t.Error("an unreported claim must not claim to have stats")
	}
	if !strings.Contains(v.Message, "driver") {
		t.Errorf("message should point at the storage driver, got %q", v.Message)
	}
}

// Every claim from one driver being silent, while another driver in the same
// cluster reports normally, is the driver — not the claims.
func TestSilentDriverIsNamedWhenNoneOfItsClaimsReport(t *testing.T) {
	vols := []model.Volume{
		{Provisioner: "csi-vxflexos.dellemc.com", Status: model.StatusUnreported, Message: "generic"},
		{Provisioner: "csi-vxflexos.dellemc.com", Status: model.StatusUnreported, Message: "generic"},
		{Provisioner: "rancher.io/local-path", Status: model.StatusOK, HasStats: true},
	}

	annotateSilentDrivers(vols)

	for _, v := range vols[:2] {
		if !strings.Contains(v.Message, "csi-vxflexos.dellemc.com") {
			t.Errorf("message does not name the driver: %q", v.Message)
		}
		if !strings.Contains(v.Message, "GET_VOLUME_STATS") {
			t.Errorf("message does not name the capability: %q", v.Message)
		}
		if !strings.Contains(v.Message, "0 of 2") {
			t.Errorf("message does not carry the evidence: %q", v.Message)
		}
	}
	if vols[2].Message != "" {
		t.Errorf("a reporting claim was annotated: %q", vols[2].Message)
	}
}

// One silent claim among many that report is a claim-level problem, so blaming
// the driver there would be a confident wrong answer.
func TestDriverThatReportsElsewhereIsNotBlamed(t *testing.T) {
	vols := []model.Volume{
		{Provisioner: "csi.trident.netapp.io", Status: model.StatusUnreported, Message: "generic"},
		{Provisioner: "csi.trident.netapp.io", Status: model.StatusOK, HasStats: true},
	}

	annotateSilentDrivers(vols)

	if vols[0].Message != "generic" {
		t.Errorf("message was replaced with a driver claim: %q", vols[0].Message)
	}
}

// A claim nobody mounts has no node, so a failure elsewhere in the cluster must
// not be pinned on it.
func TestUnmountedClaimIsUnaffectedByNodeFailures(t *testing.T) {
	c := &Collector{}
	cl := &cluster.Cluster{Name: "prod"}
	scrape := scrapeResult{
		samples: map[string]volumeSample{},
		failed:  map[string]string{"node-1": "node is not Ready"},
	}

	v := c.buildVolume(cl, boundPVC("team", "idle"), nil, map[string]mountInfo{}, scrape, 0)

	if v.Status != model.StatusUnmounted {
		t.Fatalf("status = %q, want %q", v.Status, model.StatusUnmounted)
	}
	if !strings.Contains(v.Message, "no running pod") {
		t.Errorf("message = %q", v.Message)
	}
}

// A denied nodes/proxy call is the most common cause and looks nothing like a
// storage problem, so it must not surface as a raw client-go error.
func TestForbiddenScrapeNamesTheRBACProblem(t *testing.T) {
	err := apierrors.NewForbidden(
		schema.GroupResource{Resource: "nodes"}, "node-1", nil)

	got := scrapeError(err)

	if !strings.Contains(got, "nodes/proxy") {
		t.Errorf("scrapeError(forbidden) = %q, want it to name nodes/proxy", got)
	}
}

// Multi-line client-go errors wrap badly in a single table cell.
func TestScrapeErrorKeepsOneLine(t *testing.T) {
	got := scrapeError(&multiLineError{})
	if strings.Contains(got, "\n") {
		t.Errorf("scrapeError kept a newline: %q", got)
	}
	if got != "dial tcp: lookup node-1" {
		t.Errorf("scrapeError = %q, want the first line", got)
	}
}

type multiLineError struct{}

func (*multiLineError) Error() string { return "dial tcp: lookup node-1\n\tcaused by: no such host" }
