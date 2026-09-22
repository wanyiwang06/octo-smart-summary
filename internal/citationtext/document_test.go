package citationtext

import "testing"

func TestNormalizeDocumentSectionMarkers(t *testing.T) {
	valid := func(n int) bool { return n >= 1 && n <= 4 }
	for _, tc := range []struct{ in, out string }{
		{"风险 [4.14.1]。", "风险 [4]。"},
		{"风险 [4.14.5, 4.14.9]。", "风险 [4]。"},
		{"风险 [4.18–19]。", "风险 [4]。"},
		{"风险 [3, §14]、[3, §14.4]。", "风险 [3]、[3]。"},
		{"风险 [3, 第14节]、[3, 14.4]。", "风险 [3]、[3]。"},
		{"无效来源 [9, §14.4]。", "无效来源 。"},
		{"合法引用 [3]，普通列表 [3,4]。", "合法引用 [3]，普通列表 [3,4]。"},
		{"日期 [2026-09-14]，标准 GB/T [50011-2010]。", "日期 [2026-09-14]，标准 GB/T [50011-2010]。"},
		{"代码 `[3, §14]` 和链接 [3.14](https://example.test)。", "代码 `[3, §14]` 和链接 [3.14](https://example.test)。"},
		{"转义 \\[3, §14]，图片 ![3.14](image.png)。", "转义 \\[3, §14]，图片 ![3.14](image.png)。"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := NormalizeDocumentSectionMarkers(tc.in, valid); got != tc.out {
				t.Fatalf("got %q; want %q", got, tc.out)
			}
		})
	}
}
