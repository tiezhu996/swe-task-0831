package main

import (
	"errors"
	"net/http"
	"os"
	"strconv"

	client "example.com/swe-precheck-client"
)

func (w *workbenchServer) precheckDesignDetails(out http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		workbenchError(out, 400, "预检编号无效")
		return
	}
	platform, err := w.newClient(r.Context())
	if err != nil {
		workbenchError(out, 503, "平台登录不可用")
		return
	}
	item, err := platform.ItemDetail(r.Context(), id)
	if err != nil || item.ID != id {
		workbenchError(out, 503, "预检详情读取失败")
		return
	}
	registry, err := client.DefaultPrecheckDesignStore()
	if err != nil {
		workbenchError(out, 409, err.Error())
		return
	}
	record, err := registry.Find(r.Context(), item.RepoURL, item.RequirementPrompt)
	if errors.Is(err, os.ErrNotExist) {
		canRecord := item.Status == "PRECHECK_PASSED"
		paths, pathErr := w.taskPaths()
		if pathErr != nil {
			canRecord = false
		}
		for _, path := range paths {
			state, err := client.NewWorkflowStore(path).Load(r.Context())
			if err != nil {
				canRecord = false
				continue
			}
			if state.Precheck.ItemID == id && state.Status != client.WorkflowProjectSelected && state.Status != client.WorkflowPrecheckPassed {
				canRecord = false
			}
		}
		writeWorkbenchJSON(out, 200, map[string]any{"recorded": false, "can_record": canRecord, "message": "暂无出题配套设计记录，之后设计新题时应一起保存四项材料"})
		return
	}
	if err != nil {
		workbenchError(out, 409, "设计记录或研究依据变化，请核查")
		return
	}
	approved := false
	if item.Status == "PRECHECK_PASSED" || item.Status == "PRECHECK_CONSUMED" {
		approved, err = registry.Bind(r.Context(), item)
		if err != nil {
			workbenchError(out, 409, err.Error())
			return
		}
	}
	writeWorkbenchJSON(out, 200, map[string]any{"recorded": true, "approved": approved, "fields": record.Materials.Fields, "baseline": record.Materials.BaselineCommit, "language": record.Language, "recorded_at": record.RecordedAt, "basis": record.Materials.Basis, "record_path": registry.RecordPath(record)})
}
func (w *workbenchServer) savePrecheckDesign(out http.ResponseWriter, r *http.Request) {
	w.prepareMu.Lock()
	defer w.prepareMu.Unlock()
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		workbenchError(out, 400, "预检编号无效")
		return
	}
	r.Body = http.MaxBytesReader(out, r.Body, 2<<20)
	var input struct {
		Language  string                 `json:"language"`
		Materials client.DesignMaterials `json:"materials"`
	}
	if decodeWorkbenchJSON(r, &input) != nil {
		workbenchError(out, 400, "需要 language 和 materials 的设计 JSON")
		return
	}
	paths, err := w.taskPaths()
	if err != nil {
		workbenchError(out, 409, "本地执行状态不可核查")
		return
	}
	for _, path := range paths {
		state, err := client.NewWorkflowStore(path).Load(r.Context())
		if err != nil {
			workbenchError(out, 409, "已有任务审计链不可核查，暂不能登记执行前设计")
			return
		}
		if state.Precheck.ItemID == id && state.Status != client.WorkflowProjectSelected && state.Status != client.WorkflowPrecheckPassed {
			workbenchError(out, 409, "本题已执行，缺失材料须走历史补建，不能登记为出题时设计")
			return
		}
	}
	platform, err := w.newClient(r.Context())
	if err != nil {
		workbenchError(out, 503, "平台登录不可用")
		return
	}
	item, err := platform.ItemDetail(r.Context(), id)
	if err != nil || item.ID != id || item.Status != "PRECHECK_PASSED" {
		workbenchError(out, 409, "需要平台实际通过且尚未执行的预检")
		return
	}
	if input.Materials.RepoURL != item.RepoURL || input.Materials.PromptSHA256 != client.TextSHA256(item.RequirementPrompt) {
		workbenchError(out, 409, "设计仓库或题面哈希与当前预检不同")
		return
	}
	schema, err := platform.SubmissionFormSchema(r.Context())
	if err != nil {
		workbenchError(out, 503, "设计字段规则不可读")
		return
	}
	registry, err := client.DefaultPrecheckDesignStore()
	if err != nil {
		workbenchError(out, 409, err.Error())
		return
	}
	record, err := registry.Save(r.Context(), item.RequirementPrompt, input.Language, input.Materials, schema)
	if err != nil {
		workbenchError(out, 409, err.Error())
		return
	}
	bound, err := registry.Bind(r.Context(), item)
	if err != nil || !bound {
		workbenchError(out, 409, "设计已记录，但预检绑定需要核查")
		return
	}
	writeWorkbenchJSON(out, 200, map[string]any{"recorded": true, "approved": true, "record_path": registry.RecordPath(record)})
}
