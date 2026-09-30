//! Paired-machine credentials only; provider authentication stays provider-owned.
use crate::windows_credentials::{validate_credentials, MachineCredential, MachineCredentialStore};
use serde::{Deserialize, Serialize};
use tauri::{
    plugin::{Builder, PluginHandle, TauriPlugin},
    AppHandle, Manager,
};

const MAX_VAULT_BYTES: usize = 128 * 1024;

struct AndroidCredentials(PluginHandle<tauri::Wry>);

#[derive(Deserialize)]
struct VaultReply {
    vault: Option<String>,
}

#[derive(Serialize)]
struct SaveRequest {
    vault: String,
}

#[derive(Deserialize, Serialize)]
struct Vault {
    version: u8,
    credentials: Vec<MachineCredential>,
}

pub(crate) fn init() -> TauriPlugin<tauri::Wry> {
    Builder::new("machine-credentials")
        .setup(|app, api| {
            let handle =
                api.register_android_plugin("tech.somewhere.sessions", "MachineCredentialsPlugin")?;
            app.manage(AndroidCredentials(handle));
            Ok(())
        })
        .build()
}

fn decode(reply: VaultReply) -> Result<MachineCredentialStore, String> {
    let Some(encoded) = reply.vault else {
        return Ok(MachineCredentialStore {
            supported: true,
            credentials: vec![],
        });
    };
    if encoded.len() > MAX_VAULT_BYTES {
        return Err("The protected machine credential store is too large. Contact support before changing saved machines.".to_string());
    }
    let vault: Vault = serde_json::from_str(&encoded).map_err(|_| {
        "The protected machine credential store cannot be read. Reopen Sessions; do not clear app data.".to_string()
    })?;
    if vault.version != 1 {
        return Err(
            "This protected machine credential store needs a newer Sessions app.".to_string(),
        );
    }
    Ok(MachineCredentialStore {
        supported: true,
        credentials: validate_credentials(vault.credentials)?,
    })
}

pub(crate) fn load(app: &AppHandle) -> Result<MachineCredentialStore, String> {
    let native = app.try_state::<AndroidCredentials>().ok_or_else(|| {
        "The Android protected credential service is unavailable. Update or reopen Sessions; saved connections have not been removed.".to_string()
    })?;
    let reply = native.0.run_mobile_plugin::<VaultReply>("loadMachineCredentials", ())
        .map_err(|_| "Sessions could not unlock the Android protected credentials. Unlock this device and reopen Sessions. Do not clear app data or saved connections.".to_string())?;
    decode(reply)
}

pub(crate) fn save(
    app: &AppHandle,
    credentials: Vec<MachineCredential>,
) -> Result<MachineCredentialStore, String> {
    let expected = validate_credentials(credentials)?;
    let vault = serde_json::to_string(&Vault {
        version: 1,
        credentials: expected.clone(),
    })
    .map_err(|_| "Sessions could not prepare the protected credentials.".to_string())?;
    if vault.len() > MAX_VAULT_BYTES {
        return Err("The protected machine credential store is too large.".to_string());
    }
    let native = app.try_state::<AndroidCredentials>().ok_or_else(|| {
        "The Android protected credential service is unavailable. Update or reopen Sessions before changing saved machines.".to_string()
    })?;
    let reply = native.0.run_mobile_plugin::<VaultReply>("saveMachineCredentials", SaveRequest { vault })
        .map_err(|_| "Sessions could not save or verify the Android protected credentials. Reopen the app before changing saved machines; saved connection metadata has not been removed.".to_string())?;
    let stored = decode(reply)?;
    if stored.credentials != expected {
        return Err("Sessions could not verify the protected Android credentials. Reopen the app before changing saved machines.".to_string());
    }
    Ok(stored)
}
