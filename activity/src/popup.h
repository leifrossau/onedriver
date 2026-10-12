// Shows, hides and positions the activity window, and exports Activity1.Toggle.
#pragma once

#include <QElapsedTimer>
#include <QObject>

class Backend;
class QQuickWindow;

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

private:
    void show();
    void position();
    void onActiveChanged();

    QQuickWindow *m_window;
    Backend *m_backend;
    bool m_layerShell = false;
    bool m_wasActive = false;    // got focus since it was shown
    QElapsedTimer m_hiddenByFocusLoss;
};
