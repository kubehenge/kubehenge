### Added

- Certificates take an ACME server's external account binding (`acme.externalAccountBinding.keyID`) with the HMAC key as a credential. A missing key gives no issuer and a `NoACMEEABCredential` event.
- The `external` issuer (`issuerName`, `issuerKind`, `issuerGroup`) uses an issuer you run, such as Vault, ADCS or a ClusterIssuer, and Kubehenge makes none of its own.

### Fixed

- Certificates from Sectigo, DigiCert, ZeroSSL, Google Trust Services and step-ca no longer fail with `externalAccountRequired`.
