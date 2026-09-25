//! Wire formats: server <-> daemon (WebSocket) and daemon <-> popup (JSON lines over stdio).

use std::path::PathBuf;

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct TypeRef {
    pub id: i64,
    pub name: String,
    pub hash: String,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Request {
    pub name: String,
    #[serde(default)]
    pub message: String,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Alert {
    pub id: i64,
    pub r#type: Option<TypeRef>,
    #[serde(default)]
    pub requests: Vec<Request>,
    #[serde(default)]
    pub presets: Vec<String>,
}

/// Server -> daemon.
#[derive(Debug, Deserialize)]
#[serde(tag = "op", rename_all = "snake_case")]
pub enum ServerMsg {
    Alert { alert: Alert },
    Cancel { alert_id: i64 },
    ManifestChanged,
}

/// Daemon -> server.
#[derive(Debug, Clone, Serialize)]
#[serde(tag = "op", rename_all = "snake_case")]
pub enum DeviceMsg {
    Ack { alert_id: i64 },
    Reply { alert_id: i64, text: String },
    Dismiss { alert_id: i64 },
}

#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct MediaPaths {
    pub gif: Option<PathBuf>,
    pub sound: Option<PathBuf>,
}

/// Daemon -> popup (stdin).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "op", rename_all = "snake_case")]
pub enum PopupIn {
    Init {
        alert: Alert,
        media: MediaPaths,
        monitor: String,
        hotkey: String,
    },
    /// A merged trigger: same alert id, updated requester list.
    Update { alert: Alert },
    /// Media finished downloading after the popup was already shown.
    Media { media: MediaPaths },
}

/// Popup -> daemon (stdout).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "op", rename_all = "snake_case")]
pub enum PopupOut {
    Reply { text: String },
    Dismiss,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_server_alert() {
        let m: ServerMsg = serde_json::from_str(
            r#"{"op":"alert","alert":{"id":3,"type":{"id":1,"name":"dinner","hash":"ab"},"requests":[{"name":"Mom","message":"food"}],"presets":["5 min"]}}"#,
        )
        .unwrap();
        match m {
            ServerMsg::Alert { alert } => {
                assert_eq!(alert.id, 3);
                assert_eq!(alert.r#type.unwrap().name, "dinner");
                assert_eq!(alert.requests[0].message, "food");
            }
            _ => panic!("wrong variant"),
        }
        let m: ServerMsg = serde_json::from_str(r#"{"op":"alert","alert":{"id":4,"type":null,"requests":[],"presets":[]}}"#).unwrap();
        assert!(matches!(m, ServerMsg::Alert { alert } if alert.r#type.is_none()));
        assert!(matches!(
            serde_json::from_str(r#"{"op":"manifest_changed"}"#).unwrap(),
            ServerMsg::ManifestChanged
        ));
    }

    #[test]
    fn serializes_device_msgs() {
        let s = serde_json::to_string(&DeviceMsg::Reply {
            alert_id: 1,
            text: "5 min".into(),
        })
        .unwrap();
        assert_eq!(s, r#"{"op":"reply","alert_id":1,"text":"5 min"}"#);
        assert_eq!(
            serde_json::to_string(&DeviceMsg::Dismiss { alert_id: 2 }).unwrap(),
            r#"{"op":"dismiss","alert_id":2}"#
        );
    }

    #[test]
    fn popup_messages_round_trip() {
        let m = PopupIn::Media {
            media: MediaPaths {
                gif: Some("/a.gif".into()),
                sound: None,
            },
        };
        let back: PopupIn = serde_json::from_str(&serde_json::to_string(&m).unwrap()).unwrap();
        assert_eq!(back, m);
        let o: PopupOut = serde_json::from_str(r#"{"op":"dismiss"}"#).unwrap();
        assert_eq!(o, PopupOut::Dismiss);
    }
}
