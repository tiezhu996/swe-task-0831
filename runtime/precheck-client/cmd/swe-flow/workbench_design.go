package main

import (
	client "example.com/swe-precheck-client"
	"net/http"
	"path/filepath"
)

func (w *workbenchServer) designDetails(out http.ResponseWriter, r *http.Request) {
	path, state, err := w.loadTask(r.Context(), r.PathValue("key"))
	if err != nil {
		workbenchError(out, 409, "任务审计链不可用")
		return
	}
	var plan client.SubmissionPlan
	if state.PlanPath != "" {
		plan, err = client.ReadSubmissionPlan(state.PlanPath)
		if err != nil {
			workbenchError(out, 409, "提交计划不可读")
			return
		}
	}
	missing := []string{}
	for _, key := range client.DesignFieldKeys {
		if value, ok := plan.Payload[key].(string); !ok || value == "" {
			missing = append(missing, key)
		}
	}
	frozen := state.Verification.DesignPath != "" && client.VerifyDesignBinding(state, plan) == nil
	root := filepath.Join(filepath.Dir(path), "design")
	origin := ""
	if frozen {
		var materials client.DesignMaterials
		if client.ReadLocalJSON(state.Verification.DesignPath, &materials) == nil {
			origin = materials.Origin
		}
	}
	items, summary := rubricEvidence(path, state)
	writeWorkbenchJSON(out, 200, map[string]any{"directory": root, "rubric_path": filepath.Join(root, "rubric.yaml"), "materials_path": filepath.Join(root, "materials.json"), "draft_path": filepath.Join(filepath.Dir(path), "design-draft.json"), "missing": missing, "frozen": frozen, "origin": origin, "semantic_review": "人工核对原始源码与实际日志；自动规则通过不代表语义已核验", "rubric_items": items, "rubric": summary})
}
