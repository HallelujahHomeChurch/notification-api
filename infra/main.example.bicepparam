using './main.bicep'

param imageDigest = 'sha256:0000000000000000000000000000000000000000000000000000000000000000'
param smtpAddr = 'smtp.example.com:587'
param smtpFrom = 'noreply@alive.org.tw'
param smtpAuthenticationEnabled = true
param notificationTemplateDailyLimit = 1000
param notificationsDisabled = false
