# Axon Pulse deep-link contract

Installers register the custom URL scheme `axon-pulse://`. The v1 claim link is:

```text
axon-pulse://claim?url=https%3A%2F%2Fcontroller.example&token=spt_...
```

Only the GUI receives OS deep-link events. It forwards the controller URL and
claim token to the local service over authenticated local IPC. The GUI never
persists sensor credentials. The service validates that the controller URL is
HTTPS before enrollment and erases the one-time claim token after a successful
exchange.
