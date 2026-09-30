package templates

// Lifecycle notices contain only a reference and a link to the authenticated portal.
// Request content, internal notes and exported data never belong in email.
func renderDSREmail(templateID, locale, to string, payload map[string]string) Email {
	headings := map[string]map[string]string{
		"zh-Hant": {"account.dsr-rejected": "您的個人資料申請已作成不予提供決定", "account.dsr-extended": "您的個人資料申請准駁期限已展延", "account.dsr-received": "已收到您的個人資料申請", "account.dsr-information-required": "您的個人資料申請需要補充資訊", "account.dsr-completed": "您的個人資料申請已處理完成", "account.dsr-action-required": "您的個人資料申請需要進一步處理"},
		"zh-Hans": {"account.dsr-rejected": "您的个人资料申请已作成不予提供决定", "account.dsr-extended": "您的个人资料申请准驳期限已展延", "account.dsr-received": "已收到您的个人资料申请", "account.dsr-information-required": "您的个人资料申请需要补充信息", "account.dsr-completed": "您的个人资料申请已处理完成", "account.dsr-action-required": "您的个人资料申请需要进一步处理"},
		"en":      {"account.dsr-rejected": "A refusal decision was made on your personal data request", "account.dsr-extended": "The decision deadline for your personal data request was extended", "account.dsr-received": "We received your personal data request", "account.dsr-information-required": "Your personal data request needs more information", "account.dsr-completed": "Your personal data request has been processed", "account.dsr-action-required": "Your personal data request needs further attention"},
	}
	church, message, action, footer := "Hallelujah Home Church", "Sign in to view the current status, results, any exceptions and next steps.", "View request", "Reference: "+payload["requestId"]
	if locale == "zh-Hant" {
		church, message, action, footer = "哈利路亞家教會", "請登入查看目前進度、處理結果、保留例外及下一步。", "查看申請", "申請編號："+payload["requestId"]
	}
	if locale == "zh-Hans" {
		church, message, action, footer = "哈利路亚家教会", "请登录查看当前进度、处理结果、保留例外及下一步。", "查看申请", "申请编号："+payload["requestId"]
	}
	if templateID == "account.dsr-rejected" {
		message = map[string]string{"en": "Sign in to read the decision, its reasons and available next steps. For questions, contact support@alive.org.tw.", "zh-Hant": "請登入查看決定、理由及後續處理方式。如有疑問，請聯絡 support@alive.org.tw。", "zh-Hans": "请登录查看决定、理由及后续处理方式。如有疑问，请联系 support@alive.org.tw。"}[locale]
	}
	if templateID == "account.dsr-extended" {
		message = map[string]string{"en": "Sign in to read the extension reason and the updated decision deadline. For questions, contact support@alive.org.tw.", "zh-Hant": "請登入查看展延理由與更新後的准駁期限。如有疑問，請聯絡 support@alive.org.tw。", "zh-Hans": "请登录查看展延理由与更新后的准驳期限。如有疑问，请联系 support@alive.org.tw。"}[locale]
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
