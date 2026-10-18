// Shows, hides and positions the activity window, and exports Activity1.Toggle.
#pragma once

#include <QElapsedTimer>
#include <QObject>
#include <QPoint>
#include <optional>

class Backend;
class QQuickWindow;
class QScreen;

class Popup : public QObject
{
    Q_OBJECT
    Q_CLASSINFO("D-Bus Interface", "io.github.onedriver.Activity1")
public:
    static constexpr auto ServiceName = "io.github.onedriver.Activity";
    static constexpr auto ObjectPath = "/io/github/onedriver/Activity";

    Popup(QQuickWindow *window, Backend *backend, QObject *parent = nullptr);

public slots:
    // D-Bus: show the window if hidden, hide it if shown.
    Q_SCRIPTABLE void Toggle();
    // D-Bus: like Toggle, but opens the window next to the point (x, y) in
    // global screen coordinates, normally where the tray icon was clicked.
    Q_SCRIPTABLE void ToggleAt(int x, int y);

private:
    void toggle(std::optional<QPoint> anchor);
    void show();
    void position();
    void configureLayerShell(QScreen *screen, int left, bool bottom, bool anchored);
    void onActiveChanged();

    QQuickWindow *m_window;
    Backend *m_backend;
    std::optional<QPoint> m_anchor; // where the tray icon was clicked
    bool m_layerShell = false;
    bool m_wasActive = false;    // got focus since it was shown
    QElapsedTimer m_hiddenByFocusLoss;
};
