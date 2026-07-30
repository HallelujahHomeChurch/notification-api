targetScope = 'resourceGroup'

param containerAppName string = 'notification-api'
param actionGroupName string = 'RecommendedAlertRules-AG-1'

resource api 'Microsoft.App/containerApps@2025-01-01' existing = {
  name: containerAppName
}

resource actionGroup 'Microsoft.Insights/actionGroups@2023-01-01' existing = {
  name: actionGroupName
}

resource rateLimitedAlert 'Microsoft.Insights/metricAlerts@2018-03-01' = {
  name: 'notification-api-rate-limited'
  location: 'global'
  properties: {
    description: 'Notification API returned more than ten HTTP 429 responses in five minutes.'
    severity: 2
    enabled: true
    scopes: [
      api.id
    ]
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
              values: [
                '429'
              ]
            }
          ]
        }
      ]
    }
    actions: [
      {
        actionGroupId: actionGroup.id
      }
    ]
  }
}
