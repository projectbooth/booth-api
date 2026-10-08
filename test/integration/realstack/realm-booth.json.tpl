{
  "realm": "booth",
  "enabled": true,
  "sslRequired": "none",
  "groups": [
    {
      "name": "workspaces",
      "subGroups": [
        {
          "name": "acme",
          "subGroups": [
            {
              "name": "owner"
            },
            {
              "name": "editor"
            },
            {
              "name": "viewer"
            }
          ]
        },
        {
          "name": "globex",
          "subGroups": [
            {
              "name": "owner"
            },
            {
              "name": "editor"
            },
            {
              "name": "viewer"
            }
          ]
        }
      ]
    }
  ],
  "clients": [
    {
      "clientId": "booth-design",
      "enabled": true,
      "publicClient": true,
      "standardFlowEnabled": true,
      "directAccessGrantsEnabled": true,
      "redirectUris": [
        "*"
      ],
      "webOrigins": [
        "*"
      ],
      "attributes": {
        "pkce.code.challenge.method": "S256"
      },
      "protocolMappers": [
        {
          "name": "groups",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-group-membership-mapper",
          "config": {
            "claim.name": "groups",
            "full.path": "true",
            "id.token.claim": "true",
            "access.token.claim": "true",
            "userinfo.token.claim": "true"
          }
        },
        {
          "name": "booth-design-audience",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-audience-mapper",
          "config": {
            "included.client.audience": "booth-design",
            "id.token.claim": "false",
            "access.token.claim": "true"
          }
        }
      ]
    }
  ],
  "users": [
    {
      "username": "alice",
      "enabled": true,
      "emailVerified": true,
      "email": "alice@example.test",
      "firstName": "Alice",
      "lastName": "Test",
      "credentials": [
        {
          "type": "password",
          "value": "__TEST_PASSWORD__",
          "temporary": false
        }
      ],
      "groups": [
        "/workspaces/acme/owner"
      ]
    },
    {
      "username": "bob",
      "enabled": true,
      "emailVerified": true,
      "email": "bob@example.test",
      "firstName": "Bob",
      "lastName": "Test",
      "credentials": [
        {
          "type": "password",
          "value": "__TEST_PASSWORD__",
          "temporary": false
        }
      ],
      "groups": [
        "/workspaces/acme/editor"
      ]
    },
    {
      "username": "dave",
      "enabled": true,
      "emailVerified": true,
      "email": "dave@example.test",
      "firstName": "Dave",
      "lastName": "Test",
      "credentials": [
        {
          "type": "password",
          "value": "__TEST_PASSWORD__",
          "temporary": false
        }
      ],
      "groups": [
        "/workspaces/acme/editor"
      ]
    },
    {
      "username": "carol",
      "enabled": true,
      "emailVerified": true,
      "email": "carol@example.test",
      "firstName": "Carol",
      "lastName": "Test",
      "credentials": [
        {
          "type": "password",
          "value": "__TEST_PASSWORD__",
          "temporary": false
        }
      ],
      "groups": [
        "/workspaces/globex/owner"
      ]
    }
  ]
}
