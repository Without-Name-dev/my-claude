from __future__ import annotations

from uuid import uuid4

from test_auth import admin_token, bearer, make_client, register


def test_registration_retry_reuses_device_and_recovers_credential(tmp_path):
    client = make_client(tmp_path)
    token = admin_token(client)
    body = {"name": "DSH local device", "installationId": str(uuid4())}
    first = client.post("/connectors", headers=bearer(token), json=body)
    assert first.status_code == 200, first.text
    second = client.post("/connectors", headers=bearer(token), json=body)
    assert second.status_code == 200, second.text
    first, second = first.json(), second.json()
    assert first["connector"]["id"] == second["connector"]["id"]
    assert first["connectorToken"] != second["connectorToken"]
    assert len(client.get("/connectors", headers=bearer(token)).json()["connectors"]) == 1
    connector_id = second["connector"]["id"]
    assert client.post("/connector/auth", headers={"Authorization": f"Connector {connector_id}:{first['connectorToken']}"}).status_code == 401
    assert client.post("/connector/auth", headers={"Authorization": f"Connector {connector_id}:{second['connectorToken']}"}).status_code == 200


def test_registration_key_is_scoped_to_account_and_target_requires_ownership(tmp_path):
    client = make_client(tmp_path)
    first_token = admin_token(client)
    client.patch("/admin/settings", headers=bearer(first_token), json={"registrationOpen": True})
    second_token = register(client, "another-user").json()["accessToken"]
    body = {"name": "Same computer", "installationId": str(uuid4())}
    first = client.post("/connectors", headers=bearer(first_token), json=body).json()["connector"]
    second = client.post("/connectors", headers=bearer(second_token), json=body).json()["connector"]
    assert first["id"] != second["id"]
    assert client.get(f"/connectors/{first['id']}", headers=bearer(second_token)).status_code == 404


def test_registration_without_key_keeps_existing_create_behavior(tmp_path):
    client = make_client(tmp_path)
    token = admin_token(client)
    ids = [client.post("/connectors", headers=bearer(token), json={"name": "CLI"}).json()["connector"]["id"] for _ in range(2)]
    assert ids[0] != ids[1]


def test_deleted_installation_can_register_again_with_fresh_credentials(tmp_path):
    client = make_client(tmp_path)
    token = admin_token(client)
    body = {"name": "Deleted device", "installationId": str(uuid4())}
    first = client.post("/connectors", headers=bearer(token), json=body).json()
    connector_id = first["connector"]["id"]
    assert client.delete(f"/connectors/{connector_id}", headers=bearer(token)).status_code == 204
    assert client.get("/connectors", headers=bearer(token)).json()["connectors"] == []
    second = client.post("/connectors", headers=bearer(token), json=body)
    assert second.status_code == 200, second.text
    assert second.json()["connectorToken"] != first["connectorToken"]
    assert client.post("/connector/auth", headers={"Authorization": f"Connector {connector_id}:{first['connectorToken']}"}).status_code == 401
    assert client.post("/connector/auth", headers={"Authorization": f"Connector {connector_id}:{second.json()['connectorToken']}"}).status_code == 200
