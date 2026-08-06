#![cfg_attr(not(any(target_os = "windows", test)), allow(dead_code))]

use serde::{Deserialize, Serialize};
use std::collections::HashSet;
#[cfg(target_os = "windows")]
use std::{
    fs,
    path::{Path, PathBuf},
};

const VAULT_VERSION: u8 = 1;
const MAX_CREDENTIALS: usize = 100;
const MAX_SERVER_ID_BYTES: usize = 128;
const MAX_TOKEN_BYTES: usize = 512;
#[cfg(target_os = "windows")]
const MAX_VAULT_BYTES: u64 = 1024 * 1024;

#[derive(Clone, Debug, Deserialize, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct MachineCredential {
    pub(crate) server_id: String,
    pub(crate) token: String,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct MachineCredentialStore {
    pub(crate) supported: bool,
    pub(crate) credentials: Vec<MachineCredential>,
}

impl MachineCredentialStore {
    pub(crate) fn unsupported() -> Self {
        Self {
            supported: false,
            credentials: Vec::new(),
        }
    }

    #[cfg(target_os = "windows")]
    fn windows(credentials: Vec<MachineCredential>) -> Self {
        Self {
            supported: true,
            credentials,
        }
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq, Serialize)]
struct ProtectedCredential {
    #[serde(rename = "serverId")]
    server_id: String,
    protected: String,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq, Serialize)]
struct ProtectedVault {
    version: u8,
    credentials: Vec<ProtectedCredential>,
}

fn validate_credentials(
    credentials: Vec<MachineCredential>,
) -> Result<Vec<MachineCredential>, String> {
    if credentials.len() > MAX_CREDENTIALS {
        return Err(format!(
            "Sessions can protect at most {MAX_CREDENTIALS} saved machine credentials"
        ));
    }
    let mut seen = HashSet::with_capacity(credentials.len());
    for credential in &credentials {
        if credential.server_id.is_empty()
            || credential.server_id.len() > MAX_SERVER_ID_BYTES
            || credential.server_id.chars().any(char::is_control)
        {
            return Err("a saved machine has an invalid local identifier".to_string());
        }
        if credential.token.is_empty()
            || credential.token.len() > MAX_TOKEN_BYTES
            || credential.token.chars().any(char::is_control)
        {
            return Err("a saved machine has an invalid device credential".to_string());
        }
        if !seen.insert(credential.server_id.clone()) {
            return Err("the native credential store contains a duplicate machine".to_string());
        }
    }
    Ok(credentials)
}

fn encode_hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut encoded = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        encoded.push(HEX[(byte >> 4) as usize] as char);
        encoded.push(HEX[(byte & 0x0f) as usize] as char);
    }
    encoded
}

fn decode_hex(value: &str) -> Result<Vec<u8>, String> {
    if value.is_empty() || value.len() % 2 != 0 {
        return Err("the native credential store contains invalid protected data".to_string());
    }
    let nibble = |byte: u8| -> Option<u8> {
        match byte {
            b'0'..=b'9' => Some(byte - b'0'),
            b'a'..=b'f' => Some(byte - b'a' + 10),
            b'A'..=b'F' => Some(byte - b'A' + 10),
            _ => None,
        }
    };
    value
        .as_bytes()
        .chunks_exact(2)
        .map(|pair| {
            let high = nibble(pair[0]).ok_or_else(|| {
                "the native credential store contains invalid protected data".to_string()
            })?;
            let low = nibble(pair[1]).ok_or_else(|| {
                "the native credential store contains invalid protected data".to_string()
            })?;
            Ok((high << 4) | low)
        })
        .collect()
}

fn protect_vault<F>(
    credentials: Vec<MachineCredential>,
    mut protect: F,
) -> Result<ProtectedVault, String>
where
    F: FnMut(&str, &[u8]) -> Result<Vec<u8>, String>,
{
    let credentials = validate_credentials(credentials)?;
    let protected = credentials
        .into_iter()
        .map(|credential| {
            let bytes = protect(&credential.server_id, credential.token.as_bytes())?;
            Ok(ProtectedCredential {
                server_id: credential.server_id,
                protected: encode_hex(&bytes),
            })
        })
        .collect::<Result<Vec<_>, String>>()?;
    Ok(ProtectedVault {
        version: VAULT_VERSION,
        credentials: protected,
    })
}

fn unprotect_vault<F>(
    vault: ProtectedVault,
    mut unprotect: F,
) -> Result<Vec<MachineCredential>, String>
where
    F: FnMut(&str, &[u8]) -> Result<Vec<u8>, String>,
{
    if vault.version != VAULT_VERSION {
        return Err(format!(
            "the native credential store uses unsupported version {}",
            vault.version
        ));
    }
    if vault.credentials.len() > MAX_CREDENTIALS {
        return Err("the native credential store contains too many machines".to_string());
    }
    let mut credentials = Vec::with_capacity(vault.credentials.len());
    for credential in vault.credentials {
        let protected = decode_hex(&credential.protected)?;
        let plaintext = unprotect(&credential.server_id, &protected)?;
        let token = String::from_utf8(plaintext)
            .map_err(|_| "a protected machine credential is not valid UTF-8".to_string())?;
        credentials.push(MachineCredential {
            server_id: credential.server_id,
            token,
        });
    }
    validate_credentials(credentials)
}

#[cfg(target_os = "windows")]
fn read_vault(path: &Path) -> Result<Option<ProtectedVault>, String> {
    let metadata = match fs::metadata(path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => {
            return Err(format!(
                "inspect the Windows machine credential store: {error}"
            ))
        }
    };
    if metadata.len() > MAX_VAULT_BYTES {
        return Err("the Windows machine credential store is unexpectedly large".to_string());
    }
    let bytes = fs::read(path)
        .map_err(|error| format!("read the Windows machine credential store: {error}"))?;
    serde_json::from_slice(&bytes)
        .map(Some)
        .map_err(|error| format!("parse the Windows machine credential store: {error}"))
}

#[cfg(target_os = "windows")]
fn entropy(server_id: &str) -> Vec<u8> {
    let mut value = b"Sessions machine credential v1\0".to_vec();
    value.extend_from_slice(server_id.as_bytes());
    value
}

#[cfg(target_os = "windows")]
fn dpapi_protect(server_id: &str, plaintext: &[u8]) -> Result<Vec<u8>, String> {
    use std::ptr;
    use windows_sys::Win32::{
        Foundation::{GetLastError, LocalFree},
        Security::Cryptography::{CryptProtectData, CRYPTPROTECT_UI_FORBIDDEN, CRYPT_INTEGER_BLOB},
    };

    let entropy = entropy(server_id);
    let input = CRYPT_INTEGER_BLOB {
        cbData: plaintext.len() as u32,
        pbData: plaintext.as_ptr() as *mut u8,
    };
    let entropy = CRYPT_INTEGER_BLOB {
        cbData: entropy.len() as u32,
        pbData: entropy.as_ptr() as *mut u8,
    };
    let mut output = CRYPT_INTEGER_BLOB {
        cbData: 0,
        pbData: ptr::null_mut(),
    };
    let ok = unsafe {
        CryptProtectData(
            &input,
            ptr::null(),
            &entropy,
            ptr::null(),
            ptr::null(),
            CRYPTPROTECT_UI_FORBIDDEN,
            &mut output,
        )
    };
    if ok == 0 {
        return Err(format!(
            "protect the machine credential with user-scope DPAPI (Windows error {})",
            unsafe { GetLastError() }
        ));
    }
    if output.pbData.is_null() || output.cbData == 0 {
        if !output.pbData.is_null() {
            unsafe {
                let _ = LocalFree(output.pbData as *mut _);
            }
        }
        return Err("user-scope DPAPI returned an empty protected credential".to_string());
    }
    let protected = unsafe {
        let bytes = std::slice::from_raw_parts(output.pbData, output.cbData as usize).to_vec();
        let _ = LocalFree(output.pbData as *mut _);
        bytes
    };
    Ok(protected)
}

#[cfg(target_os = "windows")]
fn dpapi_unprotect(server_id: &str, protected: &[u8]) -> Result<Vec<u8>, String> {
    use std::ptr;
    use windows_sys::Win32::{
        Foundation::{GetLastError, LocalFree},
        Security::Cryptography::{
            CryptUnprotectData, CRYPTPROTECT_UI_FORBIDDEN, CRYPT_INTEGER_BLOB,
        },
    };

    let entropy = entropy(server_id);
    let input = CRYPT_INTEGER_BLOB {
        cbData: protected.len() as u32,
        pbData: protected.as_ptr() as *mut u8,
    };
    let entropy = CRYPT_INTEGER_BLOB {
        cbData: entropy.len() as u32,
        pbData: entropy.as_ptr() as *mut u8,
    };
    let mut output = CRYPT_INTEGER_BLOB {
        cbData: 0,
        pbData: ptr::null_mut(),
    };
    let ok = unsafe {
        CryptUnprotectData(
            &input,
            ptr::null_mut(),
            &entropy,
            ptr::null(),
            ptr::null(),
            CRYPTPROTECT_UI_FORBIDDEN,
            &mut output,
        )
    };
    if ok == 0 {
        return Err(format!(
            "unlock a machine credential for this signed-in Windows user (Windows error {})",
            unsafe { GetLastError() }
        ));
    }
    if output.pbData.is_null() || output.cbData == 0 {
        if !output.pbData.is_null() {
            unsafe {
                let _ = LocalFree(output.pbData as *mut _);
            }
        }
        return Err("user-scope DPAPI returned an empty machine credential".to_string());
    }
    let plaintext = unsafe {
        let bytes = std::slice::from_raw_parts(output.pbData, output.cbData as usize).to_vec();
        let _ = LocalFree(output.pbData as *mut _);
        bytes
    };
    Ok(plaintext)
}

#[cfg(target_os = "windows")]
fn current_user_sid_string() -> Result<String, String> {
    use std::ptr;
    use windows_sys::Win32::{
        Foundation::{CloseHandle, GetLastError, LocalFree, ERROR_INSUFFICIENT_BUFFER, HANDLE},
        Security::{
            Authorization::ConvertSidToStringSidW, GetTokenInformation, TokenUser, TOKEN_QUERY,
            TOKEN_USER,
        },
        System::Threading::{GetCurrentProcess, OpenProcessToken},
    };

    let mut token: HANDLE = ptr::null_mut();
    if unsafe { OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &mut token) } == 0 {
        return Err(format!(
            "open the signed-in Windows user token (Windows error {})",
            unsafe { GetLastError() }
        ));
    }
    let result = (|| {
        let mut needed = 0_u32;
        unsafe {
            GetTokenInformation(token, TokenUser, ptr::null_mut(), 0, &mut needed);
        }
        if needed == 0 || unsafe { GetLastError() } != ERROR_INSUFFICIENT_BUFFER {
            return Err(format!(
                "size the signed-in Windows user identity (Windows error {})",
                unsafe { GetLastError() }
            ));
        }
        let mut buffer = vec![0_u8; needed as usize];
        if unsafe {
            GetTokenInformation(
                token,
                TokenUser,
                buffer.as_mut_ptr() as *mut _,
                needed,
                &mut needed,
            )
        } == 0
        {
            return Err(format!(
                "read the signed-in Windows user identity (Windows error {})",
                unsafe { GetLastError() }
            ));
        }
        let user = unsafe { &*(buffer.as_ptr() as *const TOKEN_USER) };
        let mut text = ptr::null_mut();
        if unsafe { ConvertSidToStringSidW(user.User.Sid, &mut text) } == 0 {
            return Err(format!(
                "format the signed-in Windows user identity (Windows error {})",
                unsafe { GetLastError() }
            ));
        }
        let value = unsafe {
            let mut len = 0;
            while *text.add(len) != 0 {
                len += 1;
            }
            let value = String::from_utf16_lossy(std::slice::from_raw_parts(text, len));
            let _ = LocalFree(text as *mut _);
            value
        };
        Ok(value)
    })();
    unsafe {
        CloseHandle(token);
    }
    result
}

#[cfg(target_os = "windows")]
fn acl_sddl(user_sid: &str, directory: bool) -> String {
    let inheritance = if directory { "OICI" } else { "" };
    format!("D:P(A;{inheritance};FA;;;SY)(A;{inheritance};FA;;;{user_sid})")
}

// Replace whatever the path inherited with a protected DACL granting full
// access to SYSTEM and the signed-in user only. Shared with windows_runtime:
// the staged daemon and runner binaries need the same owner-scoped policy the
// credential vault gets, because they are launched at every logon.
#[cfg(target_os = "windows")]
pub(crate) fn apply_owner_acl(path: &Path, directory: bool) -> Result<(), String> {
    use std::{os::windows::ffi::OsStrExt, ptr};
    use windows_sys::Win32::{
        Foundation::{GetLastError, LocalFree},
        Security::{
            Authorization::{
                ConvertStringSecurityDescriptorToSecurityDescriptorW, SetNamedSecurityInfoW,
                SDDL_REVISION_1, SE_FILE_OBJECT,
            },
            GetSecurityDescriptorDacl, DACL_SECURITY_INFORMATION,
            PROTECTED_DACL_SECURITY_INFORMATION,
        },
    };

    let sid = current_user_sid_string()?;
    let sddl: Vec<u16> = acl_sddl(&sid, directory)
        .encode_utf16()
        .chain(Some(0))
        .collect();
    let mut descriptor = ptr::null_mut();
    if unsafe {
        ConvertStringSecurityDescriptorToSecurityDescriptorW(
            sddl.as_ptr(),
            SDDL_REVISION_1,
            &mut descriptor,
            ptr::null_mut(),
        )
    } == 0
    {
        return Err(format!(
            "build the Windows access policy for {} (Windows error {})",
            path.display(),
            unsafe { GetLastError() }
        ));
    }

    let result = (|| {
        let mut present = 0;
        let mut defaulted = 0;
        let mut dacl = ptr::null_mut();
        if unsafe { GetSecurityDescriptorDacl(descriptor, &mut present, &mut dacl, &mut defaulted) }
            == 0
            || present == 0
            || dacl.is_null()
        {
            return Err(format!(
                "read the Windows access policy for {} (Windows error {})",
                path.display(),
                unsafe { GetLastError() }
            ));
        }
        // Keep `path` itself in scope: the failure message has to name the file
        // whose policy could not be restricted.
        let mut wide: Vec<u16> = path.as_os_str().encode_wide().chain(Some(0)).collect();
        let code = unsafe {
            SetNamedSecurityInfoW(
                wide.as_mut_ptr(),
                SE_FILE_OBJECT,
                DACL_SECURITY_INFORMATION | PROTECTED_DACL_SECURITY_INFORMATION,
                ptr::null_mut(),
                ptr::null_mut(),
                dacl,
                ptr::null_mut(),
            )
        };
        if code != 0 {
            return Err(format!(
                "restrict the Windows access policy for {} (Windows error {code})",
                path.display()
            ));
        }
        Ok(())
    })();
    unsafe {
        let _ = LocalFree(descriptor);
    }
    result
}

#[cfg(target_os = "windows")]
fn replace_file(source: &Path, destination: &Path) -> Result<(), String> {
    use std::os::windows::ffi::OsStrExt;
    use windows_sys::Win32::{
        Foundation::GetLastError,
        Storage::FileSystem::{MoveFileExW, MOVEFILE_REPLACE_EXISTING, MOVEFILE_WRITE_THROUGH},
    };
    let source: Vec<u16> = source.as_os_str().encode_wide().chain(Some(0)).collect();
    let destination: Vec<u16> = destination
        .as_os_str()
        .encode_wide()
        .chain(Some(0))
        .collect();
    if unsafe {
        MoveFileExW(
            source.as_ptr(),
            destination.as_ptr(),
            MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH,
        )
    } == 0
    {
        return Err(format!(
            "replace the Windows machine credential store (Windows error {})",
            unsafe { GetLastError() }
        ));
    }
    Ok(())
}

#[cfg(target_os = "windows")]
pub(crate) fn vault_path(app_local_data_dir: PathBuf) -> PathBuf {
    app_local_data_dir
        .join("credentials")
        .join("machine-credentials.json")
}

#[cfg(target_os = "windows")]
pub(crate) fn load(path: &Path) -> Result<MachineCredentialStore, String> {
    if path.exists() {
        let parent = path.parent().ok_or_else(|| {
            "the Windows machine credential store has no parent directory".to_string()
        })?;
        apply_owner_acl(parent, true)?;
        apply_owner_acl(path, false)?;
    }
    let credentials = match read_vault(path)? {
        Some(vault) => unprotect_vault(vault, dpapi_unprotect)?,
        None => Vec::new(),
    };
    Ok(MachineCredentialStore::windows(credentials))
}

#[cfg(target_os = "windows")]
pub(crate) fn save(
    path: &Path,
    credentials: Vec<MachineCredential>,
) -> Result<MachineCredentialStore, String> {
    static SAVE_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
    let _guard = SAVE_LOCK
        .lock()
        .map_err(|_| "the Windows machine credential store lock is unavailable".to_string())?;
    let expected = credentials.clone();
    let vault = protect_vault(credentials, dpapi_protect)?;
    let bytes = serde_json::to_vec_pretty(&vault)
        .map_err(|error| format!("encode the Windows machine credential store: {error}"))?;
    if bytes.len() as u64 > MAX_VAULT_BYTES {
        return Err("the Windows machine credential store is unexpectedly large".to_string());
    }

    let parent = path.parent().ok_or_else(|| {
        "the Windows machine credential store has no parent directory".to_string()
    })?;
    fs::create_dir_all(parent)
        .map_err(|error| format!("create the Windows credential directory: {error}"))?;
    apply_owner_acl(parent, true)?;

    let temporary = parent.join(format!(".machine-credentials-{}.tmp", std::process::id()));
    let write_result: Result<Vec<MachineCredential>, String> = (|| {
        let mut file = fs::File::create(&temporary)
            .map_err(|error| format!("stage the Windows machine credential store: {error}"))?;
        use std::io::Write as _;
        file.write_all(&bytes)
            .map_err(|error| format!("write the Windows machine credential store: {error}"))?;
        file.sync_all()
            .map_err(|error| format!("flush the Windows machine credential store: {error}"))?;
        drop(file);
        apply_owner_acl(&temporary, false)?;
        let staged = read_vault(&temporary)?.ok_or_else(|| {
            "the staged Windows machine credential store disappeared before verification"
                .to_string()
        })?;
        let verified = unprotect_vault(staged, dpapi_unprotect)?;
        if verified != expected {
            return Err(
                "the staged Windows machine credential store did not verify before saving"
                    .to_string(),
            );
        }
        replace_file(&temporary, path)?;
        Ok(verified)
    })();
    if write_result.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    write_result.map(MachineCredentialStore::windows)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture() -> Vec<MachineCredential> {
        vec![
            MachineCredential {
                server_id: "studio".to_string(),
                token: "device-token-one".to_string(),
            },
            MachineCredential {
                server_id: "mini".to_string(),
                token: "device-token-two".to_string(),
            },
        ]
    }

    #[test]
    fn protected_vault_round_trips_without_plaintext() {
        let vault = protect_vault(fixture(), |server_id, plaintext| {
            let mut protected = server_id.as_bytes().to_vec();
            protected.push(0);
            protected.extend(plaintext.iter().map(|byte| byte ^ 0xa5));
            Ok(protected)
        })
        .unwrap();
        let encoded = serde_json::to_string(&vault).unwrap();
        assert!(!encoded.contains("device-token-one"));
        assert!(!encoded.contains("device-token-two"));

        let credentials = unprotect_vault(vault, |server_id, protected| {
            let prefix = [server_id.as_bytes(), &[0]].concat();
            assert!(protected.starts_with(&prefix));
            Ok(protected[prefix.len()..]
                .iter()
                .map(|byte| byte ^ 0xa5)
                .collect())
        })
        .unwrap();
        assert_eq!(credentials, fixture());
    }

    #[test]
    fn credential_validation_rejects_duplicates_and_control_bytes() {
        let mut duplicate = fixture();
        duplicate.push(duplicate[0].clone());
        assert!(validate_credentials(duplicate)
            .unwrap_err()
            .contains("duplicate"));

        let invalid = vec![MachineCredential {
            server_id: "studio".to_string(),
            token: "secret\nvalue".to_string(),
        }];
        assert!(validate_credentials(invalid)
            .unwrap_err()
            .contains("invalid device credential"));
    }

    #[test]
    fn hex_decoder_rejects_truncated_or_non_hex_data() {
        assert_eq!(decode_hex("00ffA5").unwrap(), vec![0, 255, 165]);
        assert!(decode_hex("abc").is_err());
        assert!(decode_hex("zz").is_err());
    }

    #[cfg(target_os = "windows")]
    #[test]
    fn user_scope_dpapi_store_round_trips_and_hides_tokens() {
        let root = std::env::temp_dir().join(format!(
            "sessions-dpapi-test-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let path = vault_path(root.clone());
        let saved = save(&path, fixture()).unwrap();
        assert_eq!(saved.credentials, fixture());
        let raw = fs::read_to_string(&path).unwrap();
        assert!(!raw.contains("device-token-one"));
        assert!(!raw.contains("device-token-two"));
        assert_eq!(load(&path).unwrap().credentials, fixture());
        fs::remove_dir_all(root).unwrap();
    }

    #[cfg(target_os = "windows")]
    #[test]
    fn dpapi_entropy_binds_ciphertext_to_the_machine_identifier() {
        let protected = dpapi_protect("studio", b"device-token").unwrap();
        assert_eq!(
            dpapi_unprotect("studio", &protected).unwrap(),
            b"device-token"
        );
        assert!(dpapi_unprotect("different-machine", &protected).is_err());
    }

    #[cfg(target_os = "windows")]
    #[test]
    fn credential_acl_names_only_system_and_the_signed_in_user() {
        let sid = current_user_sid_string().unwrap();
        let file_acl = acl_sddl(&sid, false);
        assert_eq!(file_acl, format!("D:P(A;;FA;;;SY)(A;;FA;;;{sid})"));
        let directory_acl = acl_sddl(&sid, true);
        assert_eq!(
            directory_acl,
            format!("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;{sid})")
        );
    }
}
