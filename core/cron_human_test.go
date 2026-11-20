package core

import "testing"

func TestCronExprToHuman(t *testing.T) {
	for _, tc := range []struct {
		expr string
		lang Language
		want string
	}{
		{"*/20 * * * *", LangEnglish, "Every 20 minutes"},
		{"*/20 * * * *", LangChinese, "每 20 分钟"},
		{"*/1 * * * *", LangEnglish, "Every minute"},
		{"15 * * * *", LangEnglish, "Every hour at :15"},
		{"0 */2 * * *", LangEnglish, "Every 2 hours at :00"},
		{"0 9 * * *", LangEnglish, "Daily at 09:00"},
		{"30 10,14 * * *", LangChinese, "每天 10:30、14:30"},
		{"30 10,14 * * 1-5", LangEnglish, "Monday to Friday at 10:30, 14:30"},
		{"CRON_TZ=America/New_York 30 10,14 * * 1-5", LangChinese, "每周一至周五 10:30、14:30 (America/New_York)"},
		{"0 9 * * 1", LangEnglish, "Every Monday at 09:00"},
		{"0 9 * * 1,3,5", LangJapanese, "毎週月曜、水曜、金曜 09:00"},
		{"0 9 * * 7", LangEnglish, "Every Sunday at 09:00"},
		{"0 8 1 * *", LangEnglish, "Monthly on day 1 at 08:00"},
		{"0 8 5 3 *", LangSpanish, "Cada año, el 5 de mar a las 08:00"},
		{"0 8 5 3 *", LangChinese, "每年 3月5日 08:00"},
		// Forms it cannot describe exactly come back unchanged.
		{"*/5 9-17 * * 1-5", LangEnglish, "*/5 9-17 * * 1-5"},
		{"0 9 * * MON-FRI", LangEnglish, "0 9 * * MON-FRI"},
		{"5/10 * * * *", LangEnglish, "5/10 * * * *"},
		{"*/45 * * * *", LangEnglish, "*/45 * * * *"},
		{"*/7 * * * *", LangEnglish, "*/7 * * * *"},
		{"0 */23 * * *", LangEnglish, "0 */23 * * *"},
		{"0 */1 * * *", LangEnglish, "Every hour at :00"},
		{"0 */6 * * *", LangChinese, "每 6 小时，第 0 分"},
		{"0 9 1 * 1", LangEnglish, "0 9 1 * 1"},
		{"@daily", LangEnglish, "@daily"},
		{"0 25 * * *", LangEnglish, "0 25 * * *"},
	} {
		if got := CronExprToHuman(tc.expr, tc.lang); got != tc.want {
			t.Errorf("CronExprToHuman(%q, %s) = %q, want %q", tc.expr, tc.lang, got, tc.want)
		}
	}
}

func TestCronScheduleText(t *testing.T) {
	if got := cronScheduleText("*/20 * * * *", LangEnglish, true); got != "Every 20 minutes (`*/20 * * * *`)" {
		t.Fatalf("got %q", got)
	}
	if got := cronScheduleText("*/5 9-17 * * 1-5", LangEnglish, true); got != "`*/5 9-17 * * 1-5`" {
		t.Fatalf("got %q", got)
	}
	if got := cronScheduleText("*/20 * * * *", LangEnglish, false); got != "Every 20 minutes (*/20 * * * *)" {
		t.Fatalf("got %q", got)
	}
}
