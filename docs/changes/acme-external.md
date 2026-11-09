### Added

- Certificates take an ACME server's external account binding (`acme.externalAccountBinding.keyID`), its HMAC key stored as a credential. A missing key leaves no issuer, with a `NoACMEEABCredential` event.
- The `external` issuer (`issuerName`, `issuerKind`, `issuerGroup`) uses one you run, such as Vault, ADCS or a ClusterIssuer, in place of Kubehenge's own.

### Fixed

- Certificates from Sectigo, DigiCert, ZeroSSL, Google Trust Services and step-ca no longer fail with `externalAccountRequired`.
