using './main.bicep'

param migrationImage = 'alive.azurecr.io/alive/notification-api:main-0000000'
param runtimeImage = 'alive.azurecr.io/alive/notification-api:main-0000000'
param smtpAddr = 'smtp.example.com:587'
param smtpFrom = 'noreply@alive.org.tw'
param smtpAuthenticationEnabled = true
param notificationTemplateDailyLimit = 1000
param notificationsDisabled = false
