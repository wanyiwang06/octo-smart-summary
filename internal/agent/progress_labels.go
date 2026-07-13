package agent

// ToolLabels maps tool names to their corresponding phase and human-readable Chinese label.
// Used by SSE progress events to provide meaningful status updates to the frontend.
var ToolLabels = map[string]struct {
	Phase string
	Label string
}{
	"list_channels":             {Phase: "explore", Label: "探索频道"},
	"narrow_channels_by_topic":  {Phase: "explore", Label: "定位相关频道"},
	"find_shared_channels":      {Phase: "explore", Label: "查找共同频道"},
	"peek_channel":              {Phase: "fetch", Label: "预览频道"},
	"fetch_channel":             {Phase: "fetch", Label: "抓取消息"},
	"search_messages":           {Phase: "filter", Label: "搜索消息"},
	"filter_relevant":           {Phase: "filter", Label: "筛选相关消息"},
	"summarize_chunk":           {Phase: "map", Label: "分块总结"},
	"merge_summaries":           {Phase: "reduce", Label: "合并结果"},
	"get_current_time":          {Phase: "other", Label: "获取当前时间"},
	"extract_time_range":        {Phase: "other", Label: "解析时间范围"},
}

// GetToolLabel returns the phase and label for a given tool name.
// If the tool is not found in the map, it returns "other" as phase and the tool name as label.
func GetToolLabel(toolName string) (phase, label string) {
	if entry, ok := ToolLabels[toolName]; ok {
		return entry.Phase, entry.Label
	}
	return "other", toolName
}
