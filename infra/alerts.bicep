targetScope = 'resourceGroup'

param location string = resourceGroup().location
param apiAppName string = 'notification-api'
param workerAppName string = 'notification-worker'
param serviceBusNamespaceName string = 'alive-notifications-${uniqueString(subscription().id, resourceGroup().id)}'
param queueName string = 'notifications-email'
param logAnalyticsWorkspaceName string = 'alive-env-logs'
param actionGroupName string = 'RecommendedAlertRules-AG-1'
param runbookURL string = 'https://github.com/HallelujahHomeChurch/notification-api/blob/main/docs/runbook.md#incident-triage'

resource api 'Microsoft.App/containerApps@2025-01-01' existing = {
  name: apiAppName
}

resource worker 'Microsoft.App/containerApps@2025-01-01' existing = {
  name: workerAppName
}

resource serviceBus 'Microsoft.ServiceBus/namespaces@2024-01-01' existing = {
  name: serviceBusNamespaceName
}

resource workspace 'Microsoft.OperationalInsights/workspaces@2025-02-01' existing = {
  name: logAnalyticsWorkspaceName
}

resource actionGroup 'Microsoft.Insights/actionGroups@2023-01-01' existing = {
  name: actionGroupName
}

resource rateLimitedAlert 'Microsoft.Insights/metricAlerts@2018-03-01' = {
  name: 'notification-api-rate-limited'
  location: 'global'
  properties: {
    description: 'Owner: HHC platform. More than ten HTTP 429 responses in five minutes. Runbook: ${runbookURL}'
    severity: 2
    enabled: true
    autoMitigate: true
    scopes: [api.id]
    evaluationFrequency: 'PT1M'
    windowSize: 'PT5M'
    criteria: {
      'odata.type': 'Microsoft.Azure.Monitor.SingleResourceMultipleMetricCriteria'
      allOf: [
        {
          name: 'rateLimited'
          metricName: 'Requests'
          operator: 'GreaterThan'
          threshold: 10
          timeAggregation: 'Total'
          criterionType: 'StaticThresholdCriterion'
          dimensions: [
            {
              name: 'statusCode'
              operator: 'Include'
              values: ['429']
            }
          ]
        }
      ]
    }
    actions: [{ actionGroupId: actionGroup.id }]
  }
}

resource api5xxAlert 'Microsoft.Insights/metricAlerts@2018-03-01' = {
  name: 'notification-api-5xx'
  location: 'global'
  properties: {
    description: 'Owner: HHC platform. Notification API returned a 5xx response. Runbook: ${runbookURL}'
    severity: 2
    enabled: true
    autoMitigate: true
    scopes: [api.id]
    evaluationFrequency: 'PT1M'
    windowSize: 'PT5M'
    criteria: {
      'odata.type': 'Microsoft.Azure.Monitor.SingleResourceMultipleMetricCriteria'
      allOf: [
        {
          name: 'serverErrors'
          metricName: 'Requests'
          operator: 'GreaterThan'
          threshold: 0
          timeAggregation: 'Total'
          criterionType: 'StaticThresholdCriterion'
          dimensions: [
            {
              name: 'statusCodeCategory'
              operator: 'Include'
              values: ['5xx']
            }
          ]
        }
      ]
    }
    actions: [{ actionGroupId: actionGroup.id }]
  }
}

resource apiRestartAlert 'Microsoft.Insights/metricAlerts@2018-03-01' = {
  name: 'notification-api-restarts'
  location: 'global'
  properties: {
    description: 'Owner: HHC platform. Notification API restarted in five minutes. Runbook: ${runbookURL}'
    severity: 2
    enabled: true
    autoMitigate: true
    scopes: [api.id]
    evaluationFrequency: 'PT1M'
    windowSize: 'PT5M'
    criteria: {
      'odata.type': 'Microsoft.Azure.Monitor.SingleResourceMultipleMetricCriteria'
      allOf: [
        {
          name: 'restarts'
          metricName: 'RestartCount'
          operator: 'GreaterThan'
          threshold: 0
          timeAggregation: 'Total'
          criterionType: 'StaticThresholdCriterion'
        }
      ]
    }
    actions: [{ actionGroupId: actionGroup.id }]
  }
}

resource workerRestartAlert 'Microsoft.Insights/metricAlerts@2018-03-01' = {
  name: 'notification-worker-restarts'
  location: 'global'
  properties: {
    description: 'Owner: HHC platform. Notification worker restarted in five minutes. Runbook: ${runbookURL}'
    severity: 2
    enabled: true
    autoMitigate: true
    scopes: [worker.id]
    evaluationFrequency: 'PT1M'
    windowSize: 'PT5M'
    criteria: {
      'odata.type': 'Microsoft.Azure.Monitor.SingleResourceMultipleMetricCriteria'
      allOf: [
        {
          name: 'restarts'
          metricName: 'RestartCount'
          operator: 'GreaterThan'
          threshold: 0
          timeAggregation: 'Total'
          criterionType: 'StaticThresholdCriterion'
        }
      ]
    }
    actions: [{ actionGroupId: actionGroup.id }]
  }
}

resource deadLetterAlert 'Microsoft.Insights/metricAlerts@2018-03-01' = {
  name: 'notification-sb-deadlettered'
  location: 'global'
  properties: {
    description: 'Owner: HHC platform. Notification queue contains dead-lettered messages. Runbook: ${runbookURL}'
    severity: 1
    enabled: true
    autoMitigate: true
    scopes: [serviceBus.id]
    evaluationFrequency: 'PT1M'
    windowSize: 'PT5M'
    criteria: {
      'odata.type': 'Microsoft.Azure.Monitor.SingleResourceMultipleMetricCriteria'
      allOf: [
        {
          name: 'deadLetters'
          metricName: 'DeadletteredMessages'
          operator: 'GreaterThan'
          threshold: 0
          timeAggregation: 'Maximum'
          criterionType: 'StaticThresholdCriterion'
          dimensions: [
            {
              name: 'EntityName'
              operator: 'Include'
              values: [queueName]
            }
          ]
        }
      ]
    }
    actions: [{ actionGroupId: actionGroup.id }]
  }
}

resource backlogAlert 'Microsoft.Insights/metricAlerts@2018-03-01' = {
  name: 'notification-sb-backlog-stuck'
  location: 'global'
  properties: {
    description: 'Owner: HHC platform. Notification queue has not drained for 15 minutes. Runbook: ${runbookURL}'
    severity: 2
    enabled: true
    autoMitigate: true
    scopes: [serviceBus.id]
    evaluationFrequency: 'PT5M'
    windowSize: 'PT15M'
    criteria: {
      'odata.type': 'Microsoft.Azure.Monitor.SingleResourceMultipleMetricCriteria'
      allOf: [
        {
          name: 'activeMessages'
          metricName: 'ActiveMessages'
          operator: 'GreaterThan'
          threshold: 0
          timeAggregation: 'Minimum'
          criterionType: 'StaticThresholdCriterion'
          dimensions: [
            {
              name: 'EntityName'
              operator: 'Include'
              values: [queueName]
            }
          ]
        }
      ]
    }
    actions: [{ actionGroupId: actionGroup.id }]
  }
}

resource serviceBusServerErrorAlert 'Microsoft.Insights/metricAlerts@2018-03-01' = {
  name: 'notification-sb-server-errors'
  location: 'global'
  properties: {
    description: 'Owner: HHC platform. Service Bus reported a server error. Runbook: ${runbookURL}'
    severity: 1
    enabled: true
    autoMitigate: true
    scopes: [serviceBus.id]
    evaluationFrequency: 'PT1M'
    windowSize: 'PT5M'
    criteria: {
      'odata.type': 'Microsoft.Azure.Monitor.SingleResourceMultipleMetricCriteria'
      allOf: [
        {
          name: 'serverErrors'
          metricName: 'ServerErrors'
          operator: 'GreaterThan'
          threshold: 0
          timeAggregation: 'Total'
          criterionType: 'StaticThresholdCriterion'
          dimensions: [
            {
              name: 'EntityName'
              operator: 'Include'
              values: [queueName]
            }
          ]
        }
      ]
    }
    actions: [{ actionGroupId: actionGroup.id }]
  }
}

resource serviceBusThrottleAlert 'Microsoft.Insights/metricAlerts@2018-03-01' = {
  name: 'notification-sb-throttled'
  location: 'global'
  properties: {
    description: 'Owner: HHC platform. Service Bus throttled more than five requests in five minutes. Runbook: ${runbookURL}'
    severity: 2
    enabled: true
    autoMitigate: true
    scopes: [serviceBus.id]
    evaluationFrequency: 'PT1M'
    windowSize: 'PT5M'
    criteria: {
      'odata.type': 'Microsoft.Azure.Monitor.SingleResourceMultipleMetricCriteria'
      allOf: [
        {
          name: 'throttled'
          metricName: 'ThrottledRequests'
          operator: 'GreaterThan'
          threshold: 5
          timeAggregation: 'Total'
          criterionType: 'StaticThresholdCriterion'
          dimensions: [
            {
              name: 'EntityName'
              operator: 'Include'
              values: [queueName]
            }
          ]
        }
      ]
    }
    actions: [{ actionGroupId: actionGroup.id }]
  }
}

resource acceptanceUnknownAlert 'Microsoft.Insights/scheduledQueryRules@2023-12-01' = {
  name: 'notification-acceptance-unknown'
  location: location
  kind: 'LogAlert'
  properties: {
    displayName: 'Notification SMTP acceptance unknown'
    description: 'Owner: HHC platform. SMTP acceptance is unknown and may be retried. Runbook: ${runbookURL}'
    severity: 2
    enabled: true
    autoMitigate: true
    checkWorkspaceAlertsStorageConfigured: false
    skipQueryValidation: false
    evaluationFrequency: 'PT5M'
    windowSize: 'PT15M'
    scopes: [workspace.id]
    criteria: {
      allOf: [
        {
          query: '''
            ContainerAppConsoleLogs_CL
            | where ContainerAppName_s == "notification-worker"
            | where (
                Log_s has "event=notification_provider_failure"
                or Log_s has "smtp delivery failed"
              )
            | where Log_s has "kind=acceptance_unknown"
          '''
          timeAggregation: 'Count'
          operator: 'GreaterThan'
          threshold: 0
          failingPeriods: {
            numberOfEvaluationPeriods: 1
            minFailingPeriodsToAlert: 1
          }
        }
      ]
    }
    actions: {
      actionGroups: [actionGroup.id]
    }
  }
}

resource outboxDelayAlert 'Microsoft.Insights/scheduledQueryRules@2023-12-01' = {
  name: 'notification-outbox-delayed'
  location: location
  kind: 'LogAlert'
  properties: {
    displayName: 'Notification outbox delayed'
    description: 'Owner: HHC platform. An outbox item remained pending for more than five minutes. Runbook: ${runbookURL}'
    severity: 1
    enabled: true
    autoMitigate: true
    checkWorkspaceAlertsStorageConfigured: false
    skipQueryValidation: false
    evaluationFrequency: 'PT1M'
    windowSize: 'PT10M'
    scopes: [workspace.id]
    criteria: {
      allOf: [
        {
          query: '''
            ContainerAppConsoleLogs_CL
            | where ContainerAppName_s == "notification-api"
            | where Log_s has "event=notification_outbox_delayed"
          '''
          timeAggregation: 'Count'
          operator: 'GreaterThan'
          threshold: 0
          failingPeriods: {
            numberOfEvaluationPeriods: 1
            minFailingPeriodsToAlert: 1
          }
        }
      ]
    }
    actions: {
      actionGroups: [actionGroup.id]
    }
  }
}

resource providerFailureRatioAlert 'Microsoft.Insights/scheduledQueryRules@2023-12-01' = {
  name: 'notification-provider-failure-ratio'
  location: location
  kind: 'LogAlert'
  properties: {
    displayName: 'Notification provider failure ratio'
    description: 'Owner: HHC platform. At least half of 20 or more SMTP attempts failed in 15 minutes. Runbook: ${runbookURL}'
    severity: 2
    enabled: true
    autoMitigate: true
    checkWorkspaceAlertsStorageConfigured: false
    skipQueryValidation: false
    evaluationFrequency: 'PT5M'
    windowSize: 'PT15M'
    scopes: [workspace.id]
    criteria: {
      allOf: [
        {
          query: '''
            ContainerAppConsoleLogs_CL
            | where ContainerAppName_s == "notification-worker"
            | where Log_s has_any (
                "event=notification_provider_success",
                "event=notification_provider_failure",
                "smtp delivery accepted",
                "smtp delivery failed"
              )
            | summarize attempts=count(), failures=countif(
                Log_s has "event=notification_provider_failure"
                or Log_s has "smtp delivery failed"
              )
            | where attempts >= 20 and todouble(failures) / todouble(attempts) >= 0.5
          '''
          timeAggregation: 'Count'
          operator: 'GreaterThan'
          threshold: 0
          failingPeriods: {
            numberOfEvaluationPeriods: 1
            minFailingPeriodsToAlert: 1
          }
        }
      ]
    }
    actions: {
      actionGroups: [actionGroup.id]
    }
  }
}

resource providerConfigFailureAlert 'Microsoft.Insights/scheduledQueryRules@2023-12-01' = {
  name: 'notification-provider-config-failure'
  location: location
  kind: 'LogAlert'
  properties: {
    displayName: 'Notification provider configuration failure'
    description: 'Owner: HHC platform. SMTP authentication or STARTTLS failed permanently. Runbook: ${runbookURL}'
    severity: 1
    enabled: true
    autoMitigate: true
    checkWorkspaceAlertsStorageConfigured: false
    skipQueryValidation: false
    evaluationFrequency: 'PT1M'
    windowSize: 'PT5M'
    scopes: [workspace.id]
    criteria: {
      allOf: [
        {
          query: '''
            ContainerAppConsoleLogs_CL
            | where ContainerAppName_s == "notification-worker"
            | where (
                Log_s has "event=notification_provider_failure"
                or Log_s has "smtp delivery failed"
              )
            | where Log_s has_any ("operation=auth", "operation=starttls")
          '''
          timeAggregation: 'Count'
          operator: 'GreaterThan'
          threshold: 0
          failingPeriods: {
            numberOfEvaluationPeriods: 1
            minFailingPeriodsToAlert: 1
          }
        }
      ]
    }
    actions: {
      actionGroups: [actionGroup.id]
    }
  }
}

resource kedaFailureAlert 'Microsoft.Insights/scheduledQueryRules@2023-12-01' = {
  name: 'notification-worker-scaler-failure'
  location: location
  kind: 'LogAlert'
  properties: {
    displayName: 'Notification worker scaler failure'
    description: 'Owner: HHC platform. KEDA failed to check the worker scaler at least three times in 15 minutes. Runbook: ${runbookURL}'
    severity: 2
    enabled: true
    autoMitigate: true
    checkWorkspaceAlertsStorageConfigured: false
    skipQueryValidation: false
    evaluationFrequency: 'PT5M'
    windowSize: 'PT15M'
    scopes: [workspace.id]
    criteria: {
      allOf: [
        {
          query: '''
            ContainerAppSystemLogs_CL
            | where ContainerAppName_s == "notification-worker"
            | where Reason_s == "ScaledObjectCheckFailed"
            | summarize failures=count()
            | where failures >= 3
          '''
          timeAggregation: 'Count'
          operator: 'GreaterThan'
          threshold: 0
          failingPeriods: {
            numberOfEvaluationPeriods: 1
            minFailingPeriodsToAlert: 1
          }
        }
      ]
    }
    actions: {
      actionGroups: [actionGroup.id]
    }
  }
}
