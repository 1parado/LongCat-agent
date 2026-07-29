// LongCat-frontend Desktop — Tauri v2 壳。
//
// 启动时拉起 Go 后端（LongCat-frontend serve），窗口加载其 Web UI；
// 退出时结束后端进程。保持整体轻量：Rust 侧只做进程管理 + WebView。
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::net::TcpStream;
use std::path::PathBuf;
use std::process::{Child, Command};
use std::sync::Mutex;
use std::time::{Duration, Instant};

const BACKEND_ADDR: &str = "127.0.0.1:5510";

struct Backend(Mutex<Option<Child>>);

/// 依次在常见位置查找 Go 后端可执行文件。
fn find_backend() -> Option<PathBuf> {
    let exe_name = if cfg!(windows) { "LongCat-frontend.exe" } else { "LongCat-frontend" };
    let mut candidates: Vec<PathBuf> = Vec::new();
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            candidates.push(dir.join(exe_name));
            // 开发布局: desktop/src-tauri/target/{debug,release}/ -> 项目根 bin/
            candidates.push(dir.join("../../../../bin").join(exe_name));
        }
    }
    candidates.push(PathBuf::from("bin").join(exe_name));
    candidates.into_iter().find(|p| p.exists())
}

fn wait_ready(addr: &str, timeout: Duration) -> bool {
    let start = Instant::now();
    while start.elapsed() < timeout {
        if TcpStream::connect(addr).is_ok() {
            return true;
        }
        std::thread::sleep(Duration::from_millis(120));
    }
    false
}

fn main() {
    // 已有后端在运行则直接复用，否则拉起。
    let child = if TcpStream::connect(BACKEND_ADDR).is_ok() {
        None
    } else {
        let path = find_backend().expect(
            "找不到 LongCat-frontend 可执行文件，请先运行: go build -o bin/LongCat-frontend.exe ./cmd/LongCat-frontend",
        );
        let child = Command::new(path)
            .args(["serve", "-addr", BACKEND_ADDR])
            .spawn()
            .expect("启动 Go 后端失败");
        if !wait_ready(BACKEND_ADDR, Duration::from_secs(10)) {
            panic!("Go 后端在 10s 内未就绪: {BACKEND_ADDR}");
        }
        Some(child)
    };

    tauri::Builder::default()
        .manage(Backend(Mutex::new(child)))
        .on_window_event(|window, event| {
            if let tauri::WindowEvent::Destroyed = event {
                let state: tauri::State<Backend> = window.state();
                if let Some(mut c) = state.0.lock().unwrap().take() {
                    let _ = c.kill();
                }
            }
        })
        .run(tauri::generate_context!())
        .expect("Tauri 运行失败");
}
