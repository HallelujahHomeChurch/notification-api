targetScope = 'resourceGroup'

param location string = resourceGroup().location
param notificationVaultName string = 'alive-notification-${uniqueString(subscription().id, resourceGroup().id)}'
param vnetName string = 'alive-vnet'
param acaSubnetName string = 'aca'
param apiIdentityName string = 'notification-api-identity'
param workerIdentityName string = 'notification-worker-identity'
param migrateIdentityName string = 'notification-migrate-identity'

@secure()
param databaseURL string
@secure()
param dataEncryptionKey string
@secure()
param hashKey string
@secure()
param encryptionKeysJSON string
@secure()
param hashKeysJSON string
@secure()
param smtpUsername string
@secure()
param smtpPassword string

var secretsUserRole = subscriptionResourceId(
  'Microsoft.Authorization/roleDefinitions',
  '4633458b-17de-408a-b874-0445c86b69e6'
)

resource vnet 'Microsoft.Network/virtualNetworks@2024-05-01' existing = {
  name: vnetName
}

resource acaSubnet 'Microsoft.Network/virtualNetworks/subnets@2024-05-01' existing = {
  parent: vnet
  name: acaSubnetName
}

resource apiIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' existing = {
  name: apiIdentityName
}

resource workerIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' existing = {
  name: workerIdentityName
}

resource migrateIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' existing = {
  name: migrateIdentityName
}

resource vault 'Microsoft.KeyVault/vaults@2024-11-01' = {
  name: notificationVaultName
  location: location
  properties: {
    tenantId: subscription().tenantId
    sku: {
      family: 'A'
      name: 'standard'
    }
    accessPolicies: []
    enableRbacAuthorization: true
    enablePurgeProtection: true
    softDeleteRetentionInDays: 90
    publicNetworkAccess: 'Enabled'
    networkAcls: {
      bypass: 'AzureServices'
      defaultAction: 'Deny'
      virtualNetworkRules: [
        {
          id: acaSubnet.id
          ignoreMissingVnetServiceEndpoint: false
        }
      ]
      ipRules: []
    }
  }
}

resource databaseSecret 'Microsoft.KeyVault/vaults/secrets@2024-11-01' = {
  parent: vault
  name: 'notification-database-url'
  properties: {
    value: databaseURL
  }
}

resource encryptionSecret 'Microsoft.KeyVault/vaults/secrets@2024-11-01' = {
  parent: vault
  name: 'notification-data-encryption-key'
  properties: {
    value: dataEncryptionKey
  }
}

resource hashSecret 'Microsoft.KeyVault/vaults/secrets@2024-11-01' = {
  parent: vault
  name: 'notification-hash-key'
  properties: {
    value: hashKey
  }
}

resource encryptionKeysSecret 'Microsoft.KeyVault/vaults/secrets@2024-11-01' = {
  parent: vault
  name: 'notification-encryption-keys-json'
  properties: {
    value: encryptionKeysJSON
  }
}

resource hashKeysSecret 'Microsoft.KeyVault/vaults/secrets@2024-11-01' = {
  parent: vault
  name: 'notification-hash-keys-json'
  properties: {
    value: hashKeysJSON
  }
}

resource smtpUsernameSecret 'Microsoft.KeyVault/vaults/secrets@2024-11-01' = {
  parent: vault
  name: 'notification-smtp-username'
  properties: {
    value: smtpUsername
  }
}

resource smtpPasswordSecret 'Microsoft.KeyVault/vaults/secrets@2024-11-01' = {
  parent: vault
  name: 'notification-smtp-password'
  properties: {
    value: smtpPassword
  }
}

resource apiDatabaseAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(databaseSecret.id, apiIdentity.id, 'secret-read')
  scope: databaseSecret
  properties: {
    principalId: apiIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: secretsUserRole
  }
}

resource workerDatabaseAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(databaseSecret.id, workerIdentity.id, 'secret-read')
  scope: databaseSecret
  properties: {
    principalId: workerIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: secretsUserRole
  }
}

resource migrateDatabaseAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(databaseSecret.id, migrateIdentity.id, 'secret-read')
  scope: databaseSecret
  properties: {
    principalId: migrateIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: secretsUserRole
  }
}

resource apiEncryptionAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(encryptionSecret.id, apiIdentity.id, 'secret-read')
  scope: encryptionSecret
  properties: {
    principalId: apiIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: secretsUserRole
  }
}

resource workerEncryptionAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(encryptionSecret.id, workerIdentity.id, 'secret-read')
  scope: encryptionSecret
  properties: {
    principalId: workerIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: secretsUserRole
  }
}

resource apiHashAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(hashSecret.id, apiIdentity.id, 'secret-read')
  scope: hashSecret
  properties: {
    principalId: apiIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: secretsUserRole
  }
}

resource apiEncryptionKeysAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(encryptionKeysSecret.id, apiIdentity.id, 'secret-read')
  scope: encryptionKeysSecret
  properties: {
    principalId: apiIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: secretsUserRole
  }
}

resource workerEncryptionKeysAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(encryptionKeysSecret.id, workerIdentity.id, 'secret-read')
  scope: encryptionKeysSecret
  properties: {
    principalId: workerIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: secretsUserRole
  }
}

resource apiHashKeysAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(hashKeysSecret.id, apiIdentity.id, 'secret-read')
  scope: hashKeysSecret
  properties: {
    principalId: apiIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: secretsUserRole
  }
}

resource workerSMTPUsernameAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(smtpUsernameSecret.id, workerIdentity.id, 'secret-read')
  scope: smtpUsernameSecret
  properties: {
    principalId: workerIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: secretsUserRole
  }
}

resource workerSMTPPasswordAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(smtpPasswordSecret.id, workerIdentity.id, 'secret-read')
  scope: smtpPasswordSecret
  properties: {
    principalId: workerIdentity.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: secretsUserRole
  }
}

output vaultName string = vault.name
output vaultURI string = vault.properties.vaultUri
