// Talks to the onedriver instances over D-Bus and exposes the result to QML.
#pragma once

#include "models.h"

#include <QObject>
#include <QStringList>
#include <QTimer>
#include <QVariantMap>

class Backend : public QObject
{
    Q_OBJECT
    Q_PROPERTY(QStringList instanceLabels READ instanceLabels NOTIFY instancesChanged)
    Q_PROPERTY(int currentIndex READ currentIndex WRITE setCurrentIndex NOTIFY instancesChanged)
    Q_PROPERTY(bool connected READ connected NOTIFY statusChanged)
    Q_PROPERTY(QString account READ account NOTIFY statusChanged)
    Q_PROPERTY(QString stateText READ stateText NOTIFY statusChanged)
    Q_PROPERTY(QString stateIcon READ stateIcon NOTIFY statusChanged)
    Q_PROPERTY(bool checking READ checking NOTIFY statusChanged)
    Q_PROPERTY(QString checkText READ checkText NOTIFY statusChanged)
    Q_PROPERTY(bool isError READ isError NOTIFY statusChanged)
    Q_PROPERTY(bool paused READ paused NOTIFY statusChanged)
    Q_PROPERTY(QString quotaText READ quotaText NOTIFY statusChanged)
    Q_PROPERTY(double quotaFraction READ quotaFraction NOTIFY statusChanged)
    Q_PROPERTY(bool transfersSupported READ transfersSupported NOTIFY transfersSupportedChanged)
    Q_PROPERTY(TransferModel *transfers READ transfers CONSTANT)
    Q_PROPERTY(RecentModel *recent READ recent CONSTANT)

public:
    explicit Backend(QObject *parent = nullptr);

    QStringList instanceLabels() const;
    int currentIndex() const { return m_current; }
    void setCurrentIndex(int index);
    bool connected() const { return !currentService().isEmpty(); }
    QString account() const;
    QString stateText() const;
    QString stateIcon() const;
    bool isError() const;
    // a user requested check for changes is running (daemon interface v1.5, false before)
    bool checking() const { return connected() && m_status.value(QStringLiteral("Checking")).toBool(); }
    // "Checking for changes…" or "Checked 3 min ago"
    QString checkText() const;
    bool paused() const { return m_status.value(QStringLiteral("Paused")).toBool(); }
    QString quotaText() const;
    double quotaFraction() const;
    bool transfersSupported() const { return m_transfersSupported; }
    TransferModel *transfers() { return &m_transfers; }
    RecentModel *recent() { return &m_recent; }

    // Called by the window: data is only fetched and subscribed to while visible.
    void setVisible(bool visible);

    Q_INVOKABLE void togglePause();
    Q_INVOKABLE void syncNow();
    Q_INVOKABLE void openMountPoint();
    Q_INVOKABLE void openSettings();
    Q_INVOKABLE void openFile(const QString &path);   // path relative to the mountpoint
    Q_INVOKABLE void openFolder(const QString &path); // folder containing path
    Q_INVOKABLE void viewOnline(const QString &path);

signals:
    void instancesChanged();
    void statusChanged();
    void transfersSupportedChanged();

private slots:
    void onNameOwnerChanged(const QString &name, const QString &oldOwner, const QString &newOwner);
    void refresh();
    void scheduleRefresh();

private:
    struct Instance {
        QString service;
        QString label;
    };

    QString currentService() const;
    QString absPath(const QString &relative) const;
    void discover();
    void subscribe(bool on);
    void applyStatus(const QVariantMap &status);
    void fetchStatus(const QString &service);
    void fetchTransfers(const QString &service);
    void fetchRecent(const QString &service);
    void callVoid(const QString &method);

    QList<Instance> m_instances;
    int m_current = -1;
    QVariantMap m_status;
    bool m_transfersSupported = true;
    bool m_visible = false;
    QString m_subscribed; // service we are subscribed to signals of
    TransferModel m_transfers;
    RecentModel m_recent;
    QTimer m_debounce;  // coalesces signal bursts into one refresh
    QTimer m_tick;      // keeps relative times current while visible
};
