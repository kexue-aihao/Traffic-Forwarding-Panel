# Local protocol detectors

`Prepare` builds an immutable plan from effective policy layers and operator-local
profiles. TCP uses `plan.NewSession().Feed(prefix, end, "tcp")` with an append-only
prefix. A session caches rejected candidates and the next required length, with
a 65,535-byte prefix and 256-operation cryptographic budget. `Plan.Feed` starts a
fresh session and is suitable for each complete UDP datagram. Neither API sends
handshake replies, probes, or changes the inspected bytes.

`Decision` enforces authenticated SS/VMess/Trojan matches and declared structural
SOCKS/HTTP matches. Unknown or unavailable results are allowed unless the
independent unknown-deny policy is selected. Observation never claims reliable
blocking. TLS classification alone does not identify its inner application.

Local file format, passed to `LoadProfiles`:

```json
{
  "version": 1,
  "profiles": {
    "ss2017": {
      "protocol": "shadowsocks",
      "method": "aes-128-gcm",
      "password": "replace-on-agent",
      "rule_ids": ["authorized-rule-id"]
    },
    "vmess": {
      "protocol": "vmess",
      "method": "aead",
      "uuid": "0581b063-cc43-4719-bb46-736235a2c3e9"
    },
    "trojan": {"protocol": "trojan", "password": "replace-on-agent"},
    "socksudp": {"protocol": "socks5", "udp_mode": "structural"}
  }
}
```

Values shown are test placeholders. Keys/passwords, UUIDs and Trojan hashes never
belong in control-plane configuration, runtime reports or logs. Unix files must
be regular and owner-only (for example `0600`); Windows access is controlled by
the operator's file ACL. Files are limited to 1 MiB and 256 named profiles;
each plan allows at most 16 selected credentials, including identity-chain keys.
Optional `rule_ids`, `group_ids` and exact `targets` restrict local credential use.

Implemented authenticated variants:

- SS2017: `aes-128-gcm`, `aes-256-gcm`, `chacha20-ietf-poly1305`; exactly one
  password or base64 master `key`.
- SS2022: `2022-blake3-aes-128-gcm`, `2022-blake3-aes-256-gcm`,
  `2022-blake3-chacha20-poly1305`; exact-length base64 `key`, no password KDF.
  AES variants accept 2–4 colon-delimited identity/user keys for SIP023, and
  require the final AEAD header/payload authentication rather than identity alone.
- VMess AEAD: known UUID, AuthID timestamp/CRC prefilter followed by both GCM
  tags and the complete request header. TCP, UDP commands carried inside the
  stream, and mux headers are recognized even with body security `none/zero`.
- Trojan: a known password or 56-hex `trojan_hash`, complete request structure,
  and a declared plaintext observation location. Its UDP command is inside TCP.

VMess `method: legacy` is diagnostic-only: known time-window identity token and
decrypted/checksummed header are not equivalent to AEAD integrity. A strict plan
rejects that profile. SS legacy stream ciphers are unsupported for authenticated
blocking. Unknown ciphers, encrypted inner layers, excessive padding/chunk work,
unknown keys and missing observation positions remain explicit limitations.

Complete SOCKS4/4a requests and SOCKS5 client method lists are structural
evidence. SOCKS5 UDP standard headers are not identity proof: a new strict UDP
plan requires an actual trusted association or the explicit `udp_mode: structural`
profile. That mode accepts collision risk and is labelled structural. Historical
unversioned SOCKS policies retain structural UDP matching. A profile never
creates or attests a TCP UDP association.

`udp_mode: associated` additionally requires explicit `rule_ids` (UDP rules),
`targets` (their exact origin relay targets), `control_rule_ids` and a numeric
`relay_endpoint` (the reachable entry UDP IP:port). The operator-controlled SOCKS
server must return that exact endpoint in its successful BND reply. A local
registry observes completed forwarding of the actual TCP greeting, supported
method selection (no-auth or username/password), UDP ASSOCIATE and successful
BND response. It binds the original client IP and requested UDP source port;
zero source port binds the first valid UDP tuple, and ambiguous sessions remain
unconfirmed. Domain BND replies and GSSAPI/private encrypted methods are not
supported. A standard data packet's domain destination remains supported.

Preparation's `Scope.SOCKSAssociation` only declares local registry support.
It never proves that a packet is associated. The runtime must pass a live
registry-created private `association.Proof` to `Plan.FeedAssociated` for each
actual socket source tuple. Proofs bind rule, target and local generation;
TCP close, timeout, relationship/strategy change and explicit revoke invalidate
them immediately. Unassociated traffic stays unknown (default allow), rather
than being miscounted as a successful SOCKS block. A TFP exit without the real
client/control association cannot enable this mode based on entry metadata.

Production cryptography uses Go AEAD/HKDF, V2Fly's MIT-licensed VMess AEAD helpers
(`github.com/v2fly/v2ray-core/v5 v5.54.2`) and MIT-licensed BLAKE3
(`lukechampine.com/blake3 v1.4.1`). The pinned SagerNet client
(`sing-shadowsocks v0.2.9`, GPLv3) is imported exclusively by tests; production
detector/Agent dependency graphs do not include it. Required transitive notices
must accompany distributed dependencies according to their licenses.

Interop tests use the pinned clients' actual `DialConn`, `DialPacketConn` and
`ClientSession.EncodeRequestHeader` outputs, with IPv4/IPv6/domain requests,
supported ciphers, identity chains, fragmented delivery and authentication
failures. They verify protocol interoperability, not Linux throughput/PPS/P99
or 24-hour production stability.
