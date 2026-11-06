targetScope = 'resourceGroup'

@secure()
param accountCredential string

@secure()
param engagementSendCredential string

resource vault 'Microsoft.KeyVault/vaults@2026-02-01' existing = {
  name: 'alive-notify-r56q6e5qwh5'
}

var secretNames = ['notification-account-private-token', 'notification-engagement-send-token']

resource secrets 'Microsoft.KeyVault/vaults/secrets@2026-02-01' = [
  for (name, i) in secretNames: {
    parent: vault
    name: name
    properties: {
      value: i == 0 ? accountCredential : engagementSendCredential
    }
  }
]

var identityNames = ['notification-api-identity', 'account-api-identity', 'engagement-api-identity']

resource apiIdentities 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' existing = [
  for name in identityNames: {
    name: name
  }
]

var grants = [
  { secret: 0, identity: 0 }
  { secret: 0, identity: 1 }
  { secret: 1, identity: 0 }
  { secret: 1, identity: 2 }
]

resource readers 'Microsoft.Authorization/roleAssignments@2022-04-01' = [
  for grant in grants: {
    name: guid(secrets[grant.secret].id, apiIdentities[grant.identity].id, 'read')
    scope: secrets[grant.secret]
    properties: {
      principalId: apiIdentities[grant.identity].properties.principalId
      principalType: 'ServicePrincipal'
      roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '4633458b-17de-408a-b874-0445c86b69e6')
    }
  }
]
