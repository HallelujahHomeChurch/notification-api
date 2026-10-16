package templates

// Copy is closed and versioned with the template; callers supply only its purpose-bound URL.
var emailCorrectionCopy = map[string]map[string]struct{ subject, message, action, footer string }{
	"account.email-correction-verify": {
		"zh-Hant": {"驗證 Email 更正申請", "請先驗證新的 Email，之後仍須由管理員審核通過。目前的 Email 尚未變更。", "驗證新的 Email", "連結自申請時起 30 分鐘內有效；若已失效，請重新申請。請勿分享此連結。"},
		"zh-Hans": {"验证 Email 更正申请", "请先验证新的 Email，之后仍须由管理员审核通过。目前的 Email 尚未变更。", "验证新的 Email", "链接自申请时起 30 分钟内有效；若已失效，请重新申请。请勿分享此链接。"},
		"en":      {"Verify your email correction request", "Verify your new email first. Administrator approval is still required afterwards. Your current email has not changed.", "Verify new email", "This link is valid for 30 minutes from the request. If it has expired, submit a new request. Do not share this link."},
		"ja":      {"メールアドレス訂正申請の確認", "まず新しいメールアドレスを確認してください。その後、管理者の承認が必要です。現在のメールアドレスはまだ変更されていません。", "新しいメールアドレスを確認", "リンクは申請から30分間有効です。期限切れの場合は再申請してください。このリンクを共有しないでください。"},
		"ko":      {"이메일 정정 요청 인증", "먼저 새 이메일 주소를 인증해 주세요. 이후에도 관리자의 승인이 필요합니다. 현재 이메일은 아직 변경되지 않았습니다.", "새 이메일 인증", "링크는 요청 시점부터 30분 동안 유효합니다. 만료된 경우 다시 요청해 주세요. 이 링크를 공유하지 마세요."},
	},
	"account.email-correction-security-notice": {
		"zh-Hant": {"Email 更正申請安全通知", "您的 HHC 帳戶已提出 Email 更正申請。目前的 Email 尚未變更。這封通知不需要您確認或核准。", "查看帳戶安全", "若不是您本人提出，請登入帳戶檢查安全設定並聯絡教會管理員。"},
		"zh-Hans": {"Email 更正申请安全通知", "您的 HHC 帐户已提出 Email 更正申请。目前的 Email 尚未变更。这封通知不需要您确认或批准。", "查看帐户安全", "若不是您本人提出，请登录帐户检查安全设置并联系教会管理员。"},
		"en":      {"Email correction security notice", "An email correction was requested for your HHC account. Your current email has not changed. This notice requires no confirmation or approval from you.", "Review account security", "If you did not make this request, sign in to review your security settings and contact your church administrator."},
		"ja":      {"メールアドレス訂正のセキュリティ通知", "HHCアカウントのメールアドレス訂正が申請されました。現在のメールアドレスはまだ変更されていません。この通知への確認や承認は不要です。", "アカウントのセキュリティを確認", "心当たりがない場合はログインしてセキュリティ設定を確認し、教会の管理者に連絡してください。"},
		"ko":      {"이메일 정정 보안 알림", "HHC 계정의 이메일 정정이 요청되었습니다. 현재 이메일은 아직 변경되지 않았습니다. 이 알림에 대한 확인이나 승인은 필요하지 않습니다.", "계정 보안 확인", "본인이 요청하지 않았다면 로그인하여 보안 설정을 확인하고 교회 관리자에게 연락해 주세요."},
	},
	"account.email-correction-admin-review": {
		"zh-Hant": {"Email 更正申請待審核", "申請人已完成新 Email 驗證。請登入管理後台查看申請詳情，再決定是否核准。", "查看申請", "開啟連結不會核准申請。審核資格會在操作時重新確認，且不得核准自己的申請。"},
		"zh-Hans": {"Email 更正申请待审核", "申请人已完成新 Email 验证。请登录管理后台查看申请详情，再决定是否批准。", "查看申请", "打开链接不会批准申请。审核资格会在操作时重新确认，且不得批准自己的申请。"},
		"en":      {"Email correction awaiting review", "The applicant has verified their new email. Sign in to the administration portal to review the request before deciding whether to approve it.", "Review request", "Opening this link does not approve the request. Eligibility is checked again when you act, and you cannot approve your own request."},
		"ja":      {"メールアドレス訂正申請の審査待ち", "申請者が新しいメールアドレスの確認を完了しました。管理画面にログインして申請内容を確認し、承認するか判断してください。", "申請を確認", "リンクを開くだけでは承認されません。操作時に審査権限が再確認され、自分の申請は承認できません。"},
		"ko":      {"이메일 정정 요청 검토 대기", "신청자가 새 이메일 인증을 완료했습니다. 관리 페이지에 로그인하여 요청 내용을 검토한 뒤 승인 여부를 결정해 주세요.", "요청 검토", "링크를 열어도 요청이 승인되지 않습니다. 작업 시 검토 권한을 다시 확인하며 본인의 요청은 승인할 수 없습니다."},
	},
	"account.email-correction-completed": {
		"zh-Hant": {"Email 更正已完成", "您的 HHC 帳戶 Email 更正已完成。所有裝置都需要重新登入；原有的連結登入方式與密碼保持不變。", "登入帳戶", "請透過正常登入流程重新登入。"},
		"zh-Hans": {"Email 更正已完成", "您的 HHC 帐户 Email 更正已完成。所有设备都需要重新登录；原有的关联登录方式与密码保持不变。", "登录帐户", "请通过正常登录流程重新登录。"},
		"en":      {"Email correction completed", "Your HHC account email has been corrected. All devices must sign in again. Your existing linked sign-in methods and password remain unchanged.", "Sign in", "Use the normal sign-in process to access your account again."},
		"ja":      {"メールアドレスの訂正が完了しました", "HHCアカウントのメールアドレスが訂正されました。すべての端末で再ログインが必要です。既存の連携ログイン方法とパスワードは変わりません。", "ログイン", "通常のログイン手順で再度ログインしてください。"},
		"ko":      {"이메일 정정 완료", "HHC 계정의 이메일이 정정되었습니다. 모든 기기에서 다시 로그인해야 합니다. 기존에 연결된 로그인 방법과 비밀번호는 그대로 유지됩니다.", "로그인", "일반 로그인 절차를 통해 다시 로그인해 주세요."},
	},
}

func renderEmailCorrection(templateID, locale, to string, payload map[string]string) Email {
	copy := emailCorrectionCopy[templateID][locale]
	church := map[string]string{"zh-Hant": "哈利路亞家教會", "zh-Hans": "哈利路亚家教会", "en": "Hallelujah Home Church", "ja": "ハレルヤ・ホーム・チャーチ", "ko": "할렐루야 홈 교회"}[locale]
	actionURL := payload["actionUrl"]
	if templateID == "account.email-correction-verify" {
		actionURL = payload["verifyUrl"]
	}
	if templateID == "account.email-correction-admin-review" {
		actionURL = payload["reviewUrl"]
	}
	return Email{To: to, Subject: copy.subject, Body: copy.message + "\n\n" + copy.action + ": " + actionURL + "\n\n" + copy.footer + "\n", HTMLBody: brandedEmailHTMLV3(locale, church, copy.subject, copy.message, copy.action, actionURL, copy.footer)}
}
