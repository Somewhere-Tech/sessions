//! Device-local paired-machine secrets. Provider logins remain provider-owned.
use crate::windows_credentials::{validate_credentials, MachineCredential, MachineCredentialStore};
use serde::{Deserialize, Serialize};

const MAX_VAULT_BYTES: usize = 128 * 1024;
#[cfg(any(target_os = "macos", target_os = "ios"))]
const ACCOUNT: &str = "paired-machines-v1";
#[cfg(any(target_os = "macos", target_os = "ios"))]
static VAULT_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

#[derive(Serialize, Deserialize)]
struct Vault {
    version: u8,
    credentials: Vec<MachineCredential>,
}

fn decode(bytes: &[u8]) -> Result<Vec<MachineCredential>, String> {
    if bytes.len() > MAX_VAULT_BYTES {
        return Err("The protected machine credential store is too large. Contact support before changing saved machines.".to_string());
    }
    let vault: Vault = serde_json::from_slice(bytes).map_err(|_| {
        "The protected machine credential store cannot be read. Reopen Sessions; do not remove saved machines.".to_string()
    })?;
    if vault.version != 1 {
        return Err(
            "This protected machine credential store needs a newer Sessions app.".to_string(),
        );
    }
    validate_credentials(vault.credentials)
}

fn encode(credentials: Vec<MachineCredential>) -> Result<Vec<u8>, String> {
    let credentials = validate_credentials(credentials)?;
    let bytes = serde_json::to_vec(&Vault {
        version: 1,
        credentials,
    })
    .map_err(|_| "Sessions could not prepare the protected credentials.".to_string())?;
    if bytes.len() > MAX_VAULT_BYTES {
        return Err("The protected machine credential store is too large.".to_string());
    }
    Ok(bytes)
}

trait Backend {
    fn read(&self) -> Result<Option<Vec<u8>>, String>;
    fn write(&self, bytes: &[u8]) -> Result<(), String>;
}

fn read_store(backend: &impl Backend) -> Result<MachineCredentialStore, String> {
    let credentials = backend
        .read()?
        .map(|bytes| decode(&bytes))
        .transpose()?
        .unwrap_or_default();
    Ok(MachineCredentialStore {
        supported: true,
        credentials,
    })
}

fn save_store(
    backend: &impl Backend,
    credentials: Vec<MachineCredential>,
) -> Result<MachineCredentialStore, String> {
    let expected = validate_credentials(credentials)?;
    // Never overwrite an unreadable store: it may be a newer app's data.
    let previous = read_store(backend)?.credentials;
    backend.write(&encode(expected.clone())?)?;
    match read_store(backend) {
        Ok(stored) if stored.credentials == expected => Ok(stored),
        _ => {
            backend.write(&encode(previous.clone())?).map_err(|_| {
                "Sessions could not verify or restore the protected credentials. Reopen the app before changing saved machines.".to_string()
            })?;
            let restored = read_store(backend)?;
            if restored.credentials != previous {
                return Err("Sessions could not verify the restored credentials. Reopen the app before changing saved machines.".to_string());
            }
            Err("Sessions could not verify the new protected credentials; the previous set was restored. Unlock your Keychain and try again.".to_string())
        }
    }
}

#[cfg(any(target_os = "macos", target_os = "ios"))]
struct Keychain {
    service: String,
}

#[cfg(any(target_os = "macos", target_os = "ios"))]
impl Keychain {
    fn options(&self) -> security_framework::passwords::PasswordOptions {
        let mut options = security_framework::passwords::PasswordOptions::new_generic_password(
            &self.service,
            ACCOUNT,
        );
        // This is a device credential, not an iCloud-synchronized login.
        options.set_access_synchronized(Some(false));
        options
    }
}

#[cfg(any(target_os = "macos", target_os = "ios"))]
impl Backend for Keychain {
    fn read(&self) -> Result<Option<Vec<u8>>, String> {
        match security_framework::passwords::generic_password(self.options()) {
            Ok(bytes) => Ok(Some(bytes)),
            Err(error) if error.code() == -25300 => Ok(None), // errSecItemNotFound
            Err(error) => Err(format!("Sessions could not unlock the protected machine credentials (Keychain status {}). Unlock this device or its login Keychain and reopen Sessions. Saved connections have not been removed.", error.code())),
        }
    }

    fn write(&self, bytes: &[u8]) -> Result<(), String> {
        let options = self.options();
        #[cfg(target_os = "ios")]
        let options = {
            use security_framework::access_control::{ProtectionMode, SecAccessControl};
            let mut options = options;
            let control = SecAccessControl::create_with_protection(
                Some(ProtectionMode::AccessibleWhenUnlockedThisDeviceOnly),
                0,
            )
            .map_err(|_| {
                "Sessions could not configure this device's protected credential store.".to_string()
            })?;
            options.set_access_control(control);
            options
        };
        security_framework::passwords::set_generic_password_options(bytes, options)
            .map_err(|error| format!("Sessions could not save the protected machine credentials (Keychain status {}). Unlock this device or its login Keychain and try again. Sessions has not confirmed this update.", error.code()))
    }
}

#[cfg(any(target_os = "macos", target_os = "ios"))]
pub(crate) fn load(identifier: &str) -> Result<MachineCredentialStore, String> {
    let _lock = VAULT_LOCK
        .lock()
        .map_err(|_| "The protected credential worker needs an app restart.".to_string())?;
    read_store(&Keychain {
        service: format!("{identifier}.machine-credentials"),
    })
}

#[cfg(any(target_os = "macos", target_os = "ios"))]
pub(crate) fn save(
    identifier: &str,
    credentials: Vec<MachineCredential>,
) -> Result<MachineCredentialStore, String> {
    let _lock = VAULT_LOCK
        .lock()
        .map_err(|_| "The protected credential worker needs an app restart.".to_string())?;
    save_store(
        &Keychain {
            service: format!("{identifier}.machine-credentials"),
        },
        credentials,
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::{Cell, RefCell};

    #[derive(Default)]
    struct Memory {
        bytes: RefCell<Option<Vec<u8>>>,
        read_error: Cell<bool>,
        write_error: Cell<bool>,
        corrupt_once: Cell<bool>,
    }

    impl Backend for Memory {
        fn read(&self) -> Result<Option<Vec<u8>>, String> {
            if self.read_error.get() {
                return Err("vault locked".to_string());
            }
            Ok(self.bytes.borrow().clone())
        }
        fn write(&self, bytes: &[u8]) -> Result<(), String> {
            if self.write_error.get() {
                return Err("write denied".to_string());
            }
            *self.bytes.borrow_mut() = Some(if self.corrupt_once.replace(false) {
                b"invalid".to_vec()
            } else {
                bytes.to_vec()
            });
            Ok(())
        }
    }

    fn fixture() -> Vec<MachineCredential> {
        vec![MachineCredential {
            server_id: "paired-machine".to_string(),
            token: "fake-device-token".to_string(),
        }]
    }

    #[test]
    fn absent_vault_is_supported_and_save_roundtrips() {
        let backend = Memory::default();
        assert_eq!(read_store(&backend).unwrap().credentials, vec![]);
        assert_eq!(
            save_store(&backend, fixture()).unwrap().credentials,
            fixture()
        );
        assert_eq!(read_store(&backend).unwrap().credentials, fixture());
        assert!(save_store(&backend, vec![]).unwrap().credentials.is_empty());
    }

    #[test]
    fn locked_or_corrupt_store_is_not_treated_as_empty() {
        let backend = Memory::default();
        backend.read_error.set(true);
        assert!(read_store(&backend).is_err());
        assert!(save_store(&backend, fixture()).is_err());
        assert!(backend.bytes.borrow().is_none());
        backend.read_error.set(false);
        *backend.bytes.borrow_mut() = Some(b"invalid".to_vec());
        assert!(save_store(&backend, fixture()).is_err());
        assert_eq!(
            backend.bytes.borrow().as_deref(),
            Some(b"invalid".as_slice())
        );
    }

    #[test]
    fn failed_roundtrip_restores_previous_credentials() {
        let backend = Memory::default();
        save_store(&backend, fixture()).unwrap();
        backend.corrupt_once.set(true);
        assert!(save_store(&backend, vec![])
            .unwrap_err()
            .contains("previous set was restored"));
        assert_eq!(read_store(&backend).unwrap().credentials, fixture());
    }

    #[test]
    fn denied_write_preserves_previous_credentials() {
        let backend = Memory::default();
        save_store(&backend, fixture()).unwrap();
        backend.write_error.set(true);
        assert!(save_store(&backend, vec![]).is_err());
        assert_eq!(read_store(&backend).unwrap().credentials, fixture());
    }

    #[test]
    fn invalid_and_oversized_vaults_fail_without_exposing_tokens() {
        let duplicate = vec![fixture()[0].clone(), fixture()[0].clone()];
        assert!(encode(duplicate).is_err());
        assert!(decode(&vec![b' '; MAX_VAULT_BYTES + 1]).is_err());
        assert!(!decode(b"fake-device-token")
            .unwrap_err()
            .contains("fake-device-token"));
        assert!(decode(br#"{"version":2,"credentials":[]}"#)
            .unwrap_err()
            .contains("newer"));
    }
}
