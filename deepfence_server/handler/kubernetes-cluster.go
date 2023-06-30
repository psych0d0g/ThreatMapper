package handler

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/deepfence/ThreatMapper/deepfence_server/controls"
	"github.com/deepfence/ThreatMapper/deepfence_server/model"
	ctl "github.com/deepfence/ThreatMapper/deepfence_utils/controls"
	"github.com/deepfence/ThreatMapper/deepfence_utils/log"
	httpext "github.com/go-playground/pkg/v5/net/http"
)

func (h *Handler) GetKubernetesClusterControls(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	data, err := io.ReadAll(r.Body)
	if err != nil {
		respondWith(ctx, w, http.StatusBadRequest, err)
		return
	}

	var kubernetesClusterId model.AgentId

	err = json.Unmarshal(data, &kubernetesClusterId)
	if err != nil {
		respondWith(ctx, w, http.StatusBadRequest, err)
		return
	}

	actions, errs := controls.GetKubernetesClusterActions(ctx, kubernetesClusterId.NodeId, kubernetesClusterId.AvailableWorkload)
	for _, err := range errs {
		if err != nil {
			log.Warn().Msgf("Cannot some actions for %s: %v, skipping", kubernetesClusterId.NodeId, err)
		}
	}

	httpext.JSON(w, http.StatusOK, ctl.AgentControls{
		BeatRateSec: 30,
		Commands:    actions,
	})
}