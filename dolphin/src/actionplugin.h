// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once

#include <KAbstractFileItemActionPlugin>

#include <QStringList>

/// "OneDrive" submenu in the context menu of files inside an onedriver mount.
class OneDriverActionPlugin : public KAbstractFileItemActionPlugin
{
    Q_OBJECT

public:
    OneDriverActionPlugin(QObject *parent, const QVariantList &args);

    QList<QAction *> actions(const KFileItemListProperties &fileItemInfos, QWidget *parentWidget) override;

private:
    void viewOnline(const QString &path, QWidget *parent);
};
