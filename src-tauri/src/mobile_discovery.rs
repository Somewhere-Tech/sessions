use crate::NativeMobileBonjourPeer;
use mdns_sd::{ResolvedService, ServiceDaemon, ServiceEvent};
use std::{
    collections::HashMap,
    net::Ipv4Addr,
    time::{Duration, Instant},
};

const SESSIONS_SERVICE: &str = "_sessions._tcp.local.";
const BROWSE_TIMEOUT: Duration = Duration::from_secs(3);
const MAX_RESULTS: usize = 32;

pub(crate) fn browse_sessions() -> Result<Vec<NativeMobileBonjourPeer>, String> {
    let daemon = ServiceDaemon::new()
        .map_err(|error| format!("start the phone's Bonjour browser: {error}"))?;
    let receiver = daemon
        .browse(SESSIONS_SERVICE)
        .map_err(|error| format!("browse for Sessions machines: {error}"))?;
    let deadline = Instant::now() + BROWSE_TIMEOUT;
    let mut by_endpoint = HashMap::new();

    loop {
        let Some(remaining) = deadline.checked_duration_since(Instant::now()) else {
            break;
        };
        match receiver.recv_timeout(remaining) {
            Ok(ServiceEvent::ServiceResolved(service)) => {
                if let Some(peer) = peer_from_service(&service) {
                    by_endpoint.insert((peer.host.clone(), peer.port), peer);
                    if by_endpoint.len() >= MAX_RESULTS {
                        break;
                    }
                }
            }
            Ok(_) => {}
            Err(_) => break,
        }
    }

    let _ = daemon.stop_browse(SESSIONS_SERVICE);
    let _ = daemon.shutdown();
    let mut peers: Vec<_> = by_endpoint.into_values().collect();
    peers.sort_by(|left, right| {
        left.name
            .to_lowercase()
            .cmp(&right.name.to_lowercase())
            .then_with(|| left.host.cmp(&right.host))
            .then_with(|| left.port.cmp(&right.port))
    });
    Ok(peers)
}

fn peer_from_service(service: &ResolvedService) -> Option<NativeMobileBonjourPeer> {
    if !(1024..=65535).contains(&service.port) {
        return None;
    }
    let txt = service.txt_properties.clone().into_property_map_str();
    if !txt_value_is(&txt, "sessions", "1")
        || !txt_value_is(&txt, "api", "1")
        || !txt_value_is(&txt, "approval", "required")
        || !txt_value_is(&txt, "transport", "http")
    {
        return None;
    }
    let host = service
        .get_addresses_v4()
        .into_iter()
        .filter(private_lan_address)
        .min()?
        .to_string();
    let name = service_name(&service.fullname);
    Some(NativeMobileBonjourPeer {
        name,
        host,
        port: service.port,
        txt,
    })
}

fn private_lan_address(address: &Ipv4Addr) -> bool {
    address.is_private() && !address.is_loopback() && !address.is_link_local()
}

fn txt_value_is(txt: &HashMap<String, String>, key: &str, value: &str) -> bool {
    txt.iter().any(|(candidate, candidate_value)| {
        candidate.eq_ignore_ascii_case(key) && candidate_value.eq_ignore_ascii_case(value)
    })
}

fn service_name(fullname: &str) -> String {
    let suffix = format!(".{SESSIONS_SERVICE}");
    let presented = fullname
        .strip_suffix(&suffix)
        .unwrap_or(fullname)
        .trim_end_matches('.');
    let mut name = String::with_capacity(presented.len());
    let mut escaped = false;
    for character in presented.chars() {
        if escaped {
            name.push(character);
            escaped = false;
        } else if character == '\\' {
            escaped = true;
        } else {
            name.push(character);
        }
    }
    if escaped {
        name.push('\\');
    }
    let name = strip_machine_id_suffix(name.trim());
    if name.is_empty() {
        "Sessions machine".to_string()
    } else {
        name.chars().take(80).collect()
    }
}

fn strip_machine_id_suffix(name: &str) -> &str {
    let Some((machine_name, suffix)) = name.rsplit_once(" · ") else {
        return name;
    };
    if suffix.len() == 6 && suffix.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        machine_name.trim_end()
    } else {
        name
    }
}

#[cfg(test)]
mod tests {
    use super::strip_machine_id_suffix;

    #[test]
    fn hides_daemon_collision_suffix_from_people() {
        assert_eq!(
            strip_machine_id_suffix("Uzair's MacBook Pro · 9896e4"),
            "Uzair's MacBook Pro"
        );
    }

    #[test]
    fn keeps_names_without_the_daemon_suffix_shape() {
        assert_eq!(strip_machine_id_suffix("Studio · west"), "Studio · west");
        assert_eq!(strip_machine_id_suffix("Studio · 12345"), "Studio · 12345");
    }
}
