targetScope = 'resourceGroup'

@secure()
param statusCredential string

resource vault 'Microsoft.KeyVault/vaults@2026-02-01' existing = {
  name: 'alive-notify-r56q6e5qwh5'
}

resource secret 'Microsoft.KeyVault/vaults/secrets@2026-02-01' = {
  parent: vault
  name: 'notification-engagement-status-token'
  properties: {
    value: statusCredential
  }
}

resource apiIdentities 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' existing = [
  for name in ['notification-api-identity', 'engagement-api-identity']: {
    name: name
  }
]

resource readers 'Microsoft.Authorization/roleAssignments@2022-04-01' = [
  for (name, i) in ['notification-api-identity', 'engagement-api-identity']: {
    name: guid(secret.id, apiIdentities[i].id, 'read')
    scope: secret
    properties: {
      principalId: apiIdentities[i].properties.principalId
      principalType: 'ServicePrincipal'
      roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '4633458b-17de-408a-b874-0445c86b69e6')
    }
  }
]
