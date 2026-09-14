# Tenant isolation

API keys map to tenants. A tenant can only read its own jobs and artifacts. The result reference is metadata and does not grant access to the artifact.

The server derives the tenant from authentication. The Worker receives that trusted tenant with its lease. Neither a submitted document nor a payload can choose another tenant.
