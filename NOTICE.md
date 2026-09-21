# Attribution and Modification Notice

This repository is a modified edition of
[Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api), based on the official
v0.2.5 release.

The modified edition adds and maintains:

- post-hit cyber policy request-context logging;
- OpenAI TLS fingerprint selection and HTTP/2 transport handling;
- classical TLS curve filtering for the OpenAI uTLS path;
- persistent, per-account Codex turn-state ticket harvesting, scheduling,
  injection, retry policy, and administration controls.

The original project and this modified edition are distributed under the GNU
Lesser General Public License version 3. The upstream copyright notices,
license terms, and Git history are preserved. Runtime configuration, account
credentials, database contents, and production logs are not included.
