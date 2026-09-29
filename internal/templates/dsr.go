package templates

// Lifecycle notices contain only a reference and a link to the authenticated portal.
// Request content, internal notes and exported data never belong in email.
func renderDSREmail(templateID, locale, to string, payload map[string]string) Email {
	headings := map[string]map[string]string{
		"zh-Hant": {"account.dsr-received": "已收到您的個人資料申請", "account.dsr-information-required": "您的個人資料申請需要補充資訊", "account.dsr-completed": "您的個人資料申請已處理完成", "account.dsr-action-required": "您的個人資料申請需要進一步處理"},
		"zh-Hans": {"account.dsr-received": "已收到您的个人资料申请", "account.dsr-information-required": "您的个人资料申请需要补充信息", "account.dsr-completed": "您的个人资料申请已处理完成", "account.dsr-action-required": "您的个人资料申请需要进一步处理"},
		"en":      {"account.dsr-received": "We received your personal data request", "account.dsr-information-required": "Your personal data request needs more information", "account.dsr-completed": "Your personal data request has been processed", "account.dsr-action-required": "Your personal data request needs further attention"},
	}
	church, message, action, footer := "Hallelujah Home Church", "Sign in to view the current status, results, any exceptions and next steps.", "View request", "Reference: "+payload["requestId"]
	if locale == "zh-Hant" {
		church, message, action, footer = "哈利路亞家教會", "請登入查看目前進度、處理結果、保留例外及下一步。", "查看申請", "申請編號："+payload["requestId"]
	}
	if locale == "zh-Hans" {
		church, message, action, footer = "哈利路亚家教会", "请登录查看当前进度、处理结果、保留例外及下一步。", "查看申请", "申请编号："+payload["requestId"]
	}
	subject := headings[locale][templateID]
	requestURL := payload["requestUrl"]
	if templateID == "account.dsr-completed" && payload["requestType"] == "erasure" {
		action, requestURL = "", ""
		message = "Your erasure request has been processed. For the processing scope or retention exceptions, contact support@alive.org.tw."
		if locale == "zh-Hant" {
			message = "您的資料刪除申請已處理完成。如需確認處理範圍或保留例外，請聯絡 support@alive.org.tw。"
		}
		if locale == "zh-Hans" {
			message = "您的资料删除申请已处理完成。如需确认处理范围或保留例外，请联系 support@alive.org.tw。"
		}
	}
	body := message + "\n\n" + footer + "\n"
	if requestURL != "" {
		body += "\n" + action + ": " + requestURL + "\n"
	}
	return Email{To: to, Subject: subject, Body: body, HTMLBody: brandedEmailHTMLV3(locale, church, subject, message, action, requestURL, footer)}
}
