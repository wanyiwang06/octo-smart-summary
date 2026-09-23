package citationtext

import (
	"strings"
	"testing"
)

func TestNormalizeDocumentSectionMarkers(t *testing.T) {
	valid := func(n int) bool { return n >= 1 && n <= 4 }
	for _, tc := range []struct{ in, out string }{
		{"风险 [3, §14]、[3, §14.4]。", "风险 [3]、[3]。"},
		{"风险 [3, 第14节]、[3， 第14節]。", "风险 [3]、[3]。"},
		{"条款 [3, 第14条第2款]，页段 [3, 第14页]、[3, 第14段]。", "条款 [3]，页段 [3]、[3]。"},
		{"English [3, Section 14.4] [3, Sec. 14.4(b)] [3, Art. 5] [3, p.14]。", "English [3] [3] [3] [3]。"},
		{"无效来源 [9, §14.4]。", "无效来源 [9, §14.4]。"},
		{"合法引用 [3]，普通列表 [3,4]。", "合法引用 [3]，普通列表 [3,4]。"},
		{"金额 [1,234.5] 万元，页码 [3.14]万元。", "金额 [1,234.5] 万元，页码 [3.14]万元。"},
		{"Python [3.14.1]，IP [10.0.0.1]。", "Python [3.14.1]，IP [10.0.0.1]。"},
		{"区间 [1.0, 2.0]，置信区间 [2.5, 3.5]，版本 [3.14–15]。", "区间 [1.0, 2.0]，置信区间 [2.5, 3.5]，版本 [3.14–15]。"},
		{"裸章节形状 [4.14.1]、[4.18–19]、[3, 14.4]。", "裸章节形状 [4.14.1]、[4.18–19]、[3, 14.4]。"},
		{"日期 [2026-09-14]，标准 GB/T [50011-2010]。", "日期 [2026-09-14]，标准 GB/T [50011-2010]。"},
		{"代码 `[3, §14]` 和链接 [3, §14](https://example.test)。", "代码 `[3, §14]` 和链接 [3, §14](https://example.test)。"},
		{"转义 \\[3, §14]，图片 ![3, §14](image.png)。", "转义 \\[3, §14]，图片 ![3, §14](image.png)。"},
		{"预算 [1-2][3, §14] 万元。", "预算 [1-2][3, §14] 万元。"},
		{"预算 [3, §14]\t [1-2] 万元。", "预算 [3, §14]\t [1-2] 万元。"},
		{"普通引用 [1][3, §14]。", "普通引用 [1][3]。"},
		{"换行 [1-2]\n[3, §14]。", "换行 [1-2]\n[3]。"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := NormalizeDocumentSectionMarkers(tc.in, valid); got != tc.out {
				t.Fatalf("got %q; want %q", got, tc.out)
			}
		})
	}
}

func TestDocumentEvidenceForModel(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{
			"原文 [3, §14]、[3, 第十四条]、[3, Section 14.4]，引用 [3]，范围 [1-2]，列表 [1,2]，版本 [3.14.1]。",
			"原文 (3, §14)、(3, 第十四条)、(3, Section 14.4)，引用 (3)，范围 (1-2)，列表 (1,2)，版本 [3.14.1]。",
		},
		{
			"`[3]` [3](https://example.test) ![4](image.png) \\[5] 日期 [2026-09-14] GB/T [50011-2010]",
			"`(3)` (3)(https://example.test) !(4)(image.png) \\(5) 日期 (2026-09-14) GB/T (50011-2010)",
		},
	} {
		got := DocumentEvidenceForModel(tc.in)
		if got != tc.want {
			t.Fatalf("got %q; want %q", got, tc.want)
		}
		if twice := DocumentEvidenceForModel(got); twice != got {
			t.Fatalf("not idempotent: once=%q twice=%q", got, twice)
		}
	}
}

func FuzzDocumentEvidenceForModelIsIdempotent(f *testing.F) {
	for _, seed := range []string{
		"plain text",
		"[3] [1-2] [1,2] [3, §14] [3, 第十四条]",
		"`[3]` ![4](image.png) \\[5] [3.14.1]",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		once := DocumentEvidenceForModel(input)
		if twice := DocumentEvidenceForModel(once); twice != once {
			t.Fatalf("not idempotent: input=%q once=%q twice=%q", input, once, twice)
		}
	})
}

func BenchmarkNormalizeDocumentSectionMarkersLarge(b *testing.B) {
	content := strings.Repeat("条款 [3, §14.4] 普通 [1-2]\n", 16_000)
	valid := func(n int) bool { return n >= 1 && n <= 3 }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		NormalizeDocumentSectionMarkers(content, valid)
	}
}

func TestNormalizeDocumentSectionMarkersSupportsChineseNumbers(t *testing.T) {
	valid := func(n int) bool { return n == 3 }
	in := "条款 [3, 第十四条]，章节 [3, 第一章第二节]。"
	want := "条款 [3]，章节 [3]。"
	if got := NormalizeDocumentSectionMarkers(in, valid); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}
